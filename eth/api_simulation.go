package eth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/crypto/sha3"

	"github.com/ethereum/go-ethereum/core/txpool"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

var (
	enableDevnet = os.Getenv("ENABLE_DEVNET")
)

type TraceInternalTransactionArgs struct {
	Tx hexutil.Bytes `json:"tx"`
}

// CallBundleArgs represents the arguments for a call.
type CallBundleArgs struct {
	Txs                    []hexutil.Bytes       `json:"txs"`
	BlockNumber            rpc.BlockNumber       `json:"blockNumber"`
	StateBlockNumberOrHash rpc.BlockNumberOrHash `json:"stateBlockNumber"`
	Coinbase               *string               `json:"coinbase"`
	Timestamp              *uint64               `json:"timestamp"`
	Timeout                *int64                `json:"timeout"`
	GasLimit               *uint64               `json:"gasLimit"`
	Difficulty             *big.Int              `json:"difficulty"`
	BaseFee                *big.Int              `json:"baseFee"`
}

type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *txpool.TxPool
}

var (
	tracerCfgBytes []byte
)

func init() {
	tracerCfgBytes, _ = json.Marshal(map[string]interface{}{
		"withLog": true,
	})
}

// SimulationAPIBackend creates a new simulation API
type SimulationAPIBackend struct {
	eth Backend

	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription

	wg sync.WaitGroup

	exitCh chan struct{}

	// Current state for simulating the transactions at the latest block
	isCatchUpLatestBlock atomic.Bool
	currentBlock         *types.Block    // current block of the blockchain
	stateDb              *state.StateDB  // current stateDb of the blockchain
	currentSigner        types.Signer    // current signer according to the current block
	currentBlockCtx      vm.BlockContext // current block context according to the current block
}

func NewSimulationAPI(eth Backend) *SimulationAPIBackend {
	simulationAPIBackend := &SimulationAPIBackend{
		eth:         eth,
		chainHeadCh: make(chan core.ChainHeadEvent),
		exitCh:      make(chan struct{}),
	}

	// fetch the current block
	header := simulationAPIBackend.eth.BlockChain().CurrentBlock()
	if header == nil {
		panic("Failed to get the header")
	}
	block := simulationAPIBackend.eth.BlockChain().GetBlockByHash(header.Hash())
	if block == nil {
		panic("Failed to get the block by hash")
	}

	err := simulationAPIBackend.setBlock(block)
	if err != nil {
		panic(fmt.Errorf("can't init block, err: %w", err))
	}

	simulationAPIBackend.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(simulationAPIBackend.chainHeadCh)
	simulationAPIBackend.wg.Add(1)
	go func() {
		err := simulationAPIBackend.loop()
		if err != nil {
			panic("Failed to loop the simulation API main flow")
		}
	}()

	return simulationAPIBackend
}

func (b *SimulationAPIBackend) TraceInternalTransaction(_ context.Context, args TraceInternalTransactionArgs) (*types.SimulationTxResponse, error) {
	if len(args.Tx) == 0 {
		return nil, errors.New("missing transaction")
	}

	if isCatchUpLatestBlock := b.isCatchUpLatestBlock.Load(); !isCatchUpLatestBlock {
		var blockNumber uint64
		if b.currentBlock != nil {
			blockNumber = b.currentBlock.NumberU64()
		}
		return nil, fmt.Errorf("the state isn't up to date, block_number: %d", blockNumber)
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(args.Tx); err != nil {
		return nil, err
	}

	var (
		currentBlock = b.currentBlock
		stateDb      = b.stateDb
	)

	simulationResponse, err := b.simulate(tx, stateDb.Copy(), currentBlock)
	if err != nil {
		return nil, err
	}

	return simulationResponse, nil
}

func (b *SimulationAPIBackend) Stop() {
	b.chainHeadSub.Unsubscribe()
	close(b.exitCh)
	b.wg.Wait()
}

// Private methods
func (b *SimulationAPIBackend) loop() error {
	defer b.wg.Done()
	for {
		select {
		case head := <-b.chainHeadCh:
			err := b.setBlock(head.Block)
			if err != nil {
				log.Error("Failed to set block", "err", err)
				return err
			}

		case err := <-b.chainHeadSub.Err():
			return err
		case <-b.exitCh:
			return nil
		}
	}
}

func (b *SimulationAPIBackend) setBlock(currentBlock *types.Block) error {
	log.Info("Receive new head", "block", currentBlock.NumberU64())
	isEnableDevNet := enableDevnet == "true"
	blockTime := int64(currentBlock.Time())
	if !b.isLatestBlock(blockTime) && !isEnableDevNet {
		b.isCatchUpLatestBlock.Store(false)
		log.Warn("The state of the block isn't up-to-date", "block", currentBlock.NumberU64(), "time", currentBlock.Time())
		return nil
	}

	signer := types.MakeSigner(b.eth.BlockChain().Config(), currentBlock.Number(), currentBlock.Time())
	blockCtx := core.NewEVMBlockContext(currentBlock.Header(), b.eth.BlockChain(), nil)

	stateDb, err := b.eth.BlockChain().StateAt(currentBlock.Root())
	if err != nil {
		log.Error("Failed to get the read-only state of the blockchain", "hash", currentBlock.Hash().String(), "error", err)
		return err
	}
	b.stateDb = stateDb
	b.currentBlock = currentBlock
	b.currentBlockCtx = blockCtx
	b.currentSigner = signer
	b.isCatchUpLatestBlock.Store(true)
	return nil
}

// simulate the single transaction into *types.SimulationTxResponse
// use stateDb as a param, stateDb isn't safe for concurrently
// need to make the copy version of stateDb and pass as the param
func (b *SimulationAPIBackend) simulate(tx *types.Transaction, stateDb *state.StateDB, currentBlock *types.Block) (*types.SimulationTxResponse, error) {
	if tx.To() == nil {
		return nil, nil
	}

	startTraceTimeMs := time.Now().UnixMilli()

	if currentBlock == nil || currentBlock.NumberU64() <= 0 {
		return nil, fmt.Errorf("the current block is empty")
	}

	if stateDb == nil {
		return nil, fmt.Errorf("the stateDb is empty")
	}

	chainConfig := b.eth.BlockChain().Config()

	var (
		signer    = b.currentSigner
		blockCtx  = b.currentBlockCtx
		baseFee   = eip1559.CalcBaseFee(chainConfig, currentBlock.Header())
		msg, _    = core.TransactionToMessage(tx, signer, baseFee)
		txCtx     = core.NewEVMTxContext(msg)
		tracerCtx = &tracers.Context{
			BlockHash:   currentBlock.Hash(),
			BlockNumber: currentBlock.Number(),
			TxHash:      tx.Hash(),
		}
	)

	internalTransactionTracer, err := tracers.DefaultDirectory.New("callTracer", tracerCtx, tracerCfgBytes)
	if err != nil {
		log.Error("Failed to create call the tracer", "error", err)
		return nil, err
	}

	vmEVM := vm.NewEVM(blockCtx, txCtx, stateDb, chainConfig, vm.Config{
		Tracer: internalTransactionTracer,
	})

	executionResult, err := core.ApplyMessage(vmEVM, msg, new(core.GasPool).AddGas(math.MaxUint64))
	if err != nil {
		if errors.Is(err, core.ErrNonceTooLow) || errors.Is(err, core.ErrNonceMax) {
			return nil, nil
		}
		log.Error("Failed to apply the message", "hash", tx.Hash().String(), "number", currentBlock.NumberU64(), "err", err)
		return nil, err
	}

	if executionResult == nil {
		log.Warn("Simulation result is empty", "tx_hash", tx.Hash().String())
		return nil, nil
	}

	if executionResult.Failed() {
		return nil, executionResult.Err
	}

	tracerResultBytes, err := internalTransactionTracer.GetResult()
	if err != nil {
		log.Error("Failed to get the result from tracer", "err", err)
		return nil, err
	}

	if len(tracerResultBytes) == 0 {
		log.Warn("Tracer result is empty", "tx_hash", tx.Hash().String())
		return nil, nil
	}

	return &types.SimulationTxResponse{
		CallFrame: tracerResultBytes,
		DebugInfo: types.SimulationDebugInfoResponse{
			StartSimulateMs: startTraceTimeMs,
			EndSimulateMs:   time.Now().UnixMilli(),
		},
		PendingBlockNumber: currentBlock.NumberU64() + 1,
	}, nil
}

func (b *SimulationAPIBackend) isLatestBlock(blockTime int64) bool {
	secondsNow := time.Now().Unix()
	if blockTime <= secondsNow && blockTime+12 >= secondsNow {
		return true
	}
	return false
}

// CallBundle will simulate a bundle of transactions at the top of a given block
// number with the state of another (or the same) block. This can be used to
// simulate future blocks with the current state, or it can be used to simulate
// a past block.
// The sender is responsible for signing the transactions and using the correct
// nonce and ensuring validity
func (b *SimulationAPIBackend) CallBundle(ctx context.Context, args CallBundleArgs) (map[string]interface{}, error) {
	if b.currentBlock == nil {
		return nil, fmt.Errorf("the current block is empty")
	}

	if b.stateDb == nil {
		return nil, fmt.Errorf("stateDb is empty")
	}

	if isCatchUpLatestBlock := b.isCatchUpLatestBlock.Load(); !isCatchUpLatestBlock {
		var blockNumber uint64
		if b.currentBlock != nil {
			blockNumber = b.currentBlock.NumberU64()
		}
		return nil, fmt.Errorf("failed to simulate the bundle because of the state isn't up to date, block_number: %d", blockNumber)
	}

	if len(args.Txs) == 0 {
		return nil, errors.New("bundle missing txs")
	}
	if args.BlockNumber == 0 {
		return nil, errors.New("bundle missing blockNumber")
	}

	var txs types.Transactions

	for _, encodedTx := range args.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(encodedTx); err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())

	timeoutMilliSeconds := int64(5000)
	if args.Timeout != nil {
		timeoutMilliSeconds = *args.Timeout
	}
	timeout := time.Millisecond * time.Duration(timeoutMilliSeconds)

	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	var (
		stateDB      = b.stateDb.Copy()
		currentBlock = b.currentBlock
		parent       = b.currentBlock.Header()
		chainConfig  = b.eth.BlockChain().Config()
	)

	blockNumber := big.NewInt(int64(args.BlockNumber))

	timestamp := parent.Time + 1
	if args.Timestamp != nil {
		timestamp = *args.Timestamp
	}
	coinbase := parent.Coinbase
	if args.Coinbase != nil {
		coinbase = common.HexToAddress(*args.Coinbase)
	}
	difficulty := parent.Difficulty
	if args.Difficulty != nil {
		difficulty = args.Difficulty
	}
	gasLimit := parent.GasLimit
	if args.GasLimit != nil {
		gasLimit = *args.GasLimit
	}
	var baseFee *big.Int
	if args.BaseFee != nil {
		baseFee = args.BaseFee
	} else if chainConfig.IsLondon(big.NewInt(args.BlockNumber.Int64())) {
		baseFee = eip1559.CalcBaseFee(chainConfig, parent)
	}
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     blockNumber,
		GasLimit:   gasLimit,
		Time:       timestamp,
		Difficulty: difficulty,
		Coinbase:   coinbase,
		BaseFee:    baseFee,
	}

	vmconfig := vm.Config{}

	// Setup the gas pool (also for unmetered requests)
	// and apply the message.
	gp := new(core.GasPool).AddGas(math.MaxUint64)

	var results []map[string]interface{}
	coinbaseBalanceBefore := stateDB.GetBalance(coinbase)

	bundleHash := sha3.NewLegacyKeccak256()
	signer := types.MakeSigner(b.eth.BlockChain().Config(), currentBlock.Number(), currentBlock.Time())
	var totalGasUsed uint64
	gasFees := new(big.Int)
	for i, tx := range txs {
		// Check if the context was cancelled (eg. timed-out)
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		coinbaseBalanceBeforeTx := stateDB.GetBalance(coinbase)
		stateDB.SetTxContext(tx.Hash(), i)

		receipt, result, err := core.ApplyTransactionWithResult(chainConfig, b.eth.BlockChain(), &coinbase, gp, stateDB, header, tx, &header.GasUsed, vmconfig)
		if err != nil {
			return nil, fmt.Errorf("err: %w; txhash %s", err, tx.Hash())
		}

		txHash := tx.Hash().String()
		from, err := types.Sender(signer, tx)
		if err != nil {
			return nil, fmt.Errorf("err: %w; txhash %s", err, tx.Hash())
		}
		to := "0x"
		if tx.To() != nil {
			to = tx.To().String()
		}
		jsonResult := map[string]interface{}{
			"txHash":      txHash,
			"gasUsed":     receipt.GasUsed,
			"fromAddress": from.String(),
			"toAddress":   to,
		}
		totalGasUsed += receipt.GasUsed
		gasPrice, err := tx.EffectiveGasTip(header.BaseFee)
		if err != nil {
			return nil, fmt.Errorf("err: %w; txhash %s", err, tx.Hash())
		}
		gasFeesTx := new(big.Int).Mul(big.NewInt(int64(receipt.GasUsed)), gasPrice)
		gasFees.Add(gasFees, gasFeesTx)
		bundleHash.Write(tx.Hash().Bytes())
		if result.Err != nil {
			jsonResult["error"] = result.Err.Error()
			revert := result.Revert()
			if len(revert) > 0 {
				jsonResult["revert"] = string(revert)
			}
		} else {
			dst := make([]byte, hex.EncodedLen(len(result.Return())))
			hex.Encode(dst, result.Return())
			jsonResult["value"] = "0x" + string(dst)
		}
		coinbaseDiffTx := new(big.Int).Sub(stateDB.GetBalance(coinbase).ToBig(), coinbaseBalanceBeforeTx.ToBig())
		jsonResult["coinbaseDiff"] = coinbaseDiffTx.String()
		jsonResult["gasFees"] = gasFeesTx.String()
		jsonResult["ethSentToCoinbase"] = new(big.Int).Sub(coinbaseDiffTx, gasFeesTx).String()
		jsonResult["gasPrice"] = new(big.Int).Div(coinbaseDiffTx, big.NewInt(int64(receipt.GasUsed))).String()
		jsonResult["gasUsed"] = receipt.GasUsed
		results = append(results, jsonResult)
	}

	ret := map[string]interface{}{}
	ret["results"] = results
	coinbaseDiff := new(big.Int).Sub(stateDB.GetBalance(coinbase).ToBig(), coinbaseBalanceBefore.ToBig())
	ret["coinbaseDiff"] = coinbaseDiff.String()
	ret["gasFees"] = gasFees.String()
	ret["ethSentToCoinbase"] = new(big.Int).Sub(coinbaseDiff, gasFees).String()
	ret["bundleGasPrice"] = new(big.Int).Div(coinbaseDiff, big.NewInt(int64(totalGasUsed))).String()
	ret["totalGasUsed"] = totalGasUsed
	ret["stateBlockNumber"] = parent.Number.Int64()

	ret["bundleHash"] = "0x" + common.Bytes2Hex(bundleHash.Sum(nil))
	return ret, nil
}
