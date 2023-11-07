package eth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"math"
	"sync"
	"sync/atomic"
	"time"

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

type TraceInternalTransactionArgs struct {
	Tx hexutil.Bytes `json:"tx"`
}

type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *txpool.TxPool
}

// list is a "list" of the statedb belonging to an account, sorted by account nonce
type list struct {
	snapshots map[uint64]*state.StateDB
	mu        sync.Mutex
}

func (l *list) findSnapshotByNonce(nonce uint64) *state.StateDB {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshots[nonce]
}

const (
	enableCheckpointFlag = false
)

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

	stateDbCheckpoint sync.Map // store "list" of checkpoint belonging to an account address
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

			// clear the checkpoint of snapshots if the states are stale
			b.clearStaleSnapshots(readOnlyStateDb.Copy())

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
	// load the checkpoint db if exists
	var currentList *list
	if enableCheckpointFlag {
		if enc, found := b.stateDbCheckpoint.Load(msg.From.Hex()); found && enc != nil {
			list, ok := enc.(*list)
			if ok && list != nil {
				currentList = list
				checkpointStateDb := list.findSnapshotByNonce(msg.Nonce)
				if checkpointStateDb != nil {
					stateDb = checkpointStateDb.Copy()
				}
			}
		}
	}

	internalTransactionTracer, err := tracers.DefaultDirectory.New("callTracer", tracerCtx, tracerCfgBytes)
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

	if enableCheckpointFlag {
		b.storeSnapshot(stateDb, currentList, msg.Nonce, msg.From)
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
		BaseFee:            currentBlock.BaseFee(),
	}, nil
}

func (b *SimulationAPIBackend) isLatestBlock(blockTime int64) bool {
	secondsNow := time.Now().Unix()
	if blockTime <= secondsNow && blockTime >= secondsNow-12 {
		return true
	}
	return false
}

func (b *SimulationAPIBackend) storeSnapshot(stateDb *state.StateDB, l *list, nonce uint64, from common.Address) {
	if l == nil {
		l = &list{
			snapshots: make(map[uint64]*state.StateDB),
		}
	}
	nextNonce := nonce + 1
	l.snapshots[nextNonce] = stateDb
	b.stateDbCheckpoint.Store(from.Hex(), l)
}

func (b *SimulationAPIBackend) clearStaleSnapshots(stateDb *state.StateDB) {
	b.stateDbCheckpoint.Range(func(k, v any) bool {
		l := v.(*list)

		address := k.(string)

		if l == nil {
			b.stateDbCheckpoint.Delete(address)
			return true
		}

		pendingNonce := stateDb.GetNonce(common.HexToAddress(address))

		for nonce := range l.snapshots {
			if pendingNonce > nonce-1 {
				delete(l.snapshots, nonce)
			}
		}

		if len(l.snapshots) == 0 {
			b.stateDbCheckpoint.Delete(address)
			return true
		}
		return true
	})
}
