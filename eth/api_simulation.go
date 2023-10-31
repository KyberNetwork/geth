package eth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/core/txpool"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

type TraceInternalTransactionArgs struct {
	Tx hexutil.Bytes `json:"tx"`
}

type TransactionInternalTransactionsByBundleArgs struct {
	Txs []hexutil.Bytes `json:"txs"`
}

type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *txpool.TxPool
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

func (b *SimulationAPIBackend) TraceInternalTransaction(ctx context.Context, args TraceInternalTransactionArgs) (*types.SimulationTxResponse, error) {
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

func (b *SimulationAPIBackend) TraceInternalTransactionsByBundle(ctx context.Context, args TransactionInternalTransactionsByBundleArgs) ([]*types.SimulationTxResponse, error) {
	if len(args.Txs) == 0 {
		return nil, errors.New("missing transaction")
	}

	if isCatchUpLatestBlock := b.isCatchUpLatestBlock.Load(); !isCatchUpLatestBlock {
		var blockNumber uint64
		if b.currentBlock != nil {
			blockNumber = b.currentBlock.NumberU64()
		}
		return nil, fmt.Errorf("the state isn't up to date, block_number: %d", blockNumber)
	}

	var (
		currentBlock             = b.currentBlock
		stateDb                  = b.stateDb
		simulationBundleResponse = make([]*types.SimulationTxResponse, 0)
	)

	for _, binaryTx := range args.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(binaryTx); err != nil {
			return nil, err
		}

		simulationResponse, err := b.simulate(tx, stateDb.Copy(), currentBlock)
		if err != nil {
			return nil, err
		}

		simulationBundleResponse = append(simulationBundleResponse, simulationResponse)
	}

	return simulationBundleResponse, nil
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
			currentBlock := head.Block
			log.Info("Receive new head", "block", currentBlock.NumberU64())

			blockTime := int64(currentBlock.Time())
			if !b.isLatestBlock(blockTime) {
				b.isCatchUpLatestBlock.Store(false)
				log.Warn("The state of block isn't up-to-date", "block", currentBlock.NumberU64(), "time", currentBlock.Time())
				continue
			}

			signer := types.MakeSigner(b.eth.BlockChain().Config(), currentBlock.Number(), currentBlock.Time())
			blockCtx := core.NewEVMBlockContext(currentBlock.Header(), b.eth.BlockChain(), nil)

			readOnlyStateDb, err := b.eth.BlockChain().StateAt(currentBlock.Root())
			if err != nil {
				log.Error("Failed to get read-only state of the blockchain", "hash", currentBlock.Hash().String(), "error", err)
				return err
			}
			b.stateDb = readOnlyStateDb
			b.currentBlock = currentBlock
			b.currentBlockCtx = blockCtx
			b.currentSigner = signer
			b.isCatchUpLatestBlock.Store(true)
		case err := <-b.chainHeadSub.Err():
			return err
		case <-b.exitCh:
			return nil
		}
	}
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
		return nil, fmt.Errorf("current block is empty")
	}

	if stateDb == nil {
		return nil, fmt.Errorf("stateDb is empty")
	}

	chainConfig := b.eth.BlockChain().Config()

	var (
		signer    = b.currentSigner
		blockCtx  = b.currentBlockCtx
		msg, _    = core.TransactionToMessageWithSkipsBaseFeeCheck(tx, signer, currentBlock.BaseFee())
		txCtx     = core.NewEVMTxContext(msg)
		tracerCtx = &tracers.Context{
			BlockHash:   currentBlock.Hash(),
			BlockNumber: currentBlock.Number(),
			TxHash:      tx.Hash(),
		}
	)

	internalTransactionTracer, err := tracers.DefaultDirectory.New(native.InternalTransactionTracerName, tracerCtx, json.RawMessage{})
	if err != nil {
		log.Error("Failed to create call tracer", "error", err)
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

	var internalTxTracerOutput native.InternalTxTracerOutput

	if err := json.Unmarshal(tracerResultBytes, &internalTxTracerOutput); err != nil {
		log.Error("Failed to unmarshal the internal transactions tracers", "err", err)
		return nil, err
	}

	internalTxsResponse := make([]types.InternalTxResponse, 0, len(internalTxTracerOutput.InternalTxs))
	for _, internalTx := range internalTxTracerOutput.InternalTxs {
		to := ""
		value := "0"
		if internalTx.To != nil {
			to = internalTx.To.String()
		}
		if internalTx.Value != nil {
			value = internalTx.Value.String()
		}
		internalTxsResponse = append(internalTxsResponse, types.InternalTxResponse{
			Type:    internalTx.Type.String(),
			From:    strings.ToLower(internalTx.From.String()),
			To:      strings.ToLower(to),
			Gas:     internalTx.Gas,
			GasUsed: internalTx.GasUsed,
			Input:   internalTx.Input,
			Value:   value,
		})
	}
	eventLogs := make([]types.EventLogResponse, 0, len(internalTxTracerOutput.EventLogs))
	for _, eventLog := range internalTxTracerOutput.EventLogs {
		topics := make([]string, 0, len(eventLog.Topics))
		for _, topic := range eventLog.Topics {
			topics = append(topics, topic.Hex())
		}
		eventLogs = append(eventLogs, types.EventLogResponse{
			Data:    hexutil.Encode(eventLog.Data),
			Address: eventLog.Address.String(),
			Topics:  topics,
		})
	}
	return &types.SimulationTxResponse{
		InternalTxs: internalTxsResponse,
		DebugInfo: types.SimulationDebugInfoResponse{
			StartSimulateMs: startTraceTimeMs,
			EndSimulateMs:   time.Now().UnixMilli(),
		},
		PendingBlockNumber: currentBlock.NumberU64() + 1,
		BaseFee:            currentBlock.BaseFee(),
		Logs:               eventLogs,
	}, nil
}

func (b *SimulationAPIBackend) isLatestBlock(blockTime int64) bool {
	secondsNow := time.Now().Unix()
	if blockTime <= secondsNow && blockTime >= secondsNow-12 {
		return true
	}
	return false
}
