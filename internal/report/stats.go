package report

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

// TurnStat is one LLM round-trip as billed.
type TurnStat struct {
	Turn         int `json:"turn"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	ToolCalls    int `json:"tool_calls"`
}

// RunStats is the machine-readable cost record of one run, written next to
// the report (success AND failure — failed runs burn tokens too, which is
// exactly when the burn needs to be visible). The orchestrator ingests it.
type RunStats struct {
	RunStartedAt time.Time  `json:"run_started_at"`
	Outcome      string     `json:"outcome"` // submitted | failed
	FailReason   string     `json:"fail_reason,omitempty"`
	Turns        int        `json:"turns"`
	ToolCalls    int        `json:"tool_calls"`
	InputTokens  int        `json:"input_tokens"`
	OutputTokens int        `json:"output_tokens"`
	PeakInput    int        `json:"peak_input_tokens"` // largest single turn: the live context size
	PrunedBytes  int        `json:"pruned_bytes"`      // transcript bytes withheld by pruning, summed over sends
	PerTurn      []TurnStat `json:"per_turn"`
}

// StatsPath derives the stats artifact path from a report path (works for
// both 20060102-1504.md and 20060102-1504-FAILED.md).
func StatsPath(reportPath string) string {
	return strings.TrimSuffix(reportPath, ".md") + ".stats.json"
}

// WriteStats persists the run's cost record. Same O_EXCL discipline as the
// report; callers treat failure as log-worthy, never run-fatal — a run must
// not be lost because its cost record could not be written.
func WriteStats(reportPath string, st RunStats) error {
	f, err := os.OpenFile(StatsPath(reportPath), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}
