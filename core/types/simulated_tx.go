package types

import "math/big"

type SimulationDebugInfoResponse struct {
	StartSimulateMs int64 `json:"start_simulate_ms"`
	EndSimulateMs   int64 `json:"end_simulate_ms"`
}

type InternalTxResponse struct {
	Type    string `json:"type"`
	From    string `json:"from"`
	To      string `json:"to"`
	Gas     uint64 `json:"gas"`
	GasUsed uint64 `json:"gas_used"`
	Input   string `json:"input"`
	Value   string `json:"value"`
}

type EventLogResponse struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type SimulationTxResponse struct {
	PendingBlockNumber uint64                      `json:"pending_block_number"`
	BaseFee            *big.Int                    `json:"base_fee"`
	InternalTxs        []InternalTxResponse        `json:"internal_transactions"`
	DebugInfo          SimulationDebugInfoResponse `json:"debug_info"`
	Logs               []EventLogResponse          `json:"logs"`
}
