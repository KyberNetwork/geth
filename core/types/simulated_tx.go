package types

import (
	"encoding/json"
	"math/big"
)

type SimulationDebugInfoResponse struct {
	StartSimulateMs int64 `json:"start_simulate_ms"`
	EndSimulateMs   int64 `json:"end_simulate_ms"`
}

type SimulationTxResponse struct {
	PendingBlockNumber uint64                      `json:"pending_block_number"`
	BaseFee            *big.Int                    `json:"base_fee"`
	CurrentBlockTime   uint64                      `json:"current_block_time"`
	DebugInfo          SimulationDebugInfoResponse `json:"debug_info"`
	CallFrame          json.RawMessage             `json:"call_frame"`
}
