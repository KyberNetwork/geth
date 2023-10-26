package native

import (
	"encoding/json"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"math/big"
	"sync/atomic"
)

func init() {
	tracers.DefaultDirectory.Register(InternalTransactionTracerName, NewInternalTransactionTracer, false)
}

type InternalTx struct {
	Type    vm.OpCode       `json:"type"`
	From    common.Address  `json:"from"`
	To      *common.Address `json:"to"` // can be empty when the transaction is a creation tx
	Gas     uint64          `json:"gas"`
	GasUsed uint64          `json:"gas_used"`
	Input   string          `json:"input"`
	Value   *big.Int        `json:"value,omitempty"`
}

type InternalTxs []InternalTx

type InternalTxTracerOutput struct {
	InternalTxs InternalTxs `json:"internal_txs,omitempty"`
	EventLogs   []types.Log `json:"event_logs"`
}

type internalTransactionTracer struct {
	// transaction info
	gasLimit  uint64
	interrupt atomic.Bool
	err       error

	// output
	internalTransactions InternalTxs
	eventLogs            []types.Log
}

const InternalTransactionTracerName = "internalTransactionTracer"

func NewInternalTransactionTracer(ctx *tracers.Context, _ json.RawMessage) (tracers.Tracer, error) {
	return &internalTransactionTracer{}, nil
}

func (t *internalTransactionTracer) CaptureTxStart(gasLimit uint64) {
	t.gasLimit = gasLimit
}
func (t *internalTransactionTracer) CaptureTxEnd(restGas uint64) {
	t.internalTransactions[0].GasUsed = t.gasLimit - restGas
}
func (t *internalTransactionTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	toAddress := to

	var (
		opCode = vm.CALL
	)
	if create {
		opCode = vm.CREATE
	}

	t.internalTransactions = append(t.internalTransactions, InternalTx{
		Type:  opCode,
		From:  from,
		To:    &toAddress,
		Input: "0x" + common.Bytes2Hex(input),
		Gas:   t.gasLimit,
		Value: value,
	})

}
func (t *internalTransactionTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
	if err != nil {
		t.err = err
		return
	}
}

// CaptureEnter is called when EVM enters a new scope (via call, create or selfdestruct).
func (t *internalTransactionTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	if t.interrupt.Load() {
		return
	}

	if typ != vm.CALL {
		return
	}

	toCopied := to
	t.internalTransactions = append(t.internalTransactions, InternalTx{
		Type:  typ,
		From:  from,
		To:    &toCopied,
		Input: "0x" + common.Bytes2Hex(input),
		Gas:   gas,
		Value: value,
	})
}
func (t *internalTransactionTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	if err != nil {
		t.err = err
		return
	}
	size := len(t.internalTransactions)
	if size <= 0 {
		return
	}

	t.internalTransactions[size-1].Gas = gasUsed
}

// CaptureState implements the EVMLogger interface to trace a single step of VM execution.
func (t *internalTransactionTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	if err != nil {
		t.err = err
		return
	}

	switch op {
	case vm.LOG0, vm.LOG1, vm.LOG2, vm.LOG3, vm.LOG4:
		// TODO(tiennampham23): uncomment if we need the logs from the transactions
		size := int(op - vm.LOG0)

		stack := scope.Stack
		stackData := stack.Data()

		// Don't modify the stack
		mStart := stackData[len(stackData)-1]
		mSize := stackData[len(stackData)-2]
		topics := make([]common.Hash, size)
		for i := 0; i < size; i++ {
			topic := stackData[len(stackData)-2-(i+1)]
			topics[i] = topic.Bytes32()
		}

		data, err := tracers.GetMemoryCopyPadded(scope.Memory, int64(mStart.Uint64()), int64(mSize.Uint64()))
		if err != nil {
			return
		}

		t.eventLogs = append(t.eventLogs, types.Log{
			Address: scope.Contract.Address(),
			Topics:  topics,
			Data:    data,
		})
	}
}
func (t *internalTransactionTracer) CaptureFault(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, depth int, err error) {
}

func (t *internalTransactionTracer) GetResult() (json.RawMessage, error) {
	output := InternalTxTracerOutput{
		InternalTxs: t.internalTransactions,
		EventLogs:   t.eventLogs,
	}
	b, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (t *internalTransactionTracer) Stop(err error) {
	t.interrupt.Store(true)
}
