// Package loop is the directive loop: DIRECTIVE.md as the system prompt,
// ge-mcp tools + the loop-local submit_report tool, run until the model
// submits a valid report (or the turn/time budget runs out — in which case a
// FAILED report with the audit log is written, so no run is silently lost).
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/osrs-ge/ge-agent/internal/config"
	"github.com/osrs-ge/ge-agent/internal/llm"
	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
	"github.com/osrs-ge/ge-agent/internal/report"
)

const submitReportTool = "submit_report"

var submitReportDef = json.RawMessage(`{
  "name": "submit_report",
  "description": "Submit the run's final markdown report. Call exactly once, at the end. The harness validates the required sections (Header, Digest, Strategies, Proof, Discarded — in order), writes the file with a UTC timestamp name, and appends the authoritative tool-call log. Do not include the appendix yourself.",
  "input_schema": {
    "type": "object",
    "properties": {
      "markdown": {"type": "string", "description": "The complete report markdown, all five sections in order"}
    },
    "required": ["markdown"]
  }
}`)

// Run executes one directive cycle. Returns the report path.
func Run(ctx context.Context, cfg *config.Config) (string, error) {
	runStart := time.Now().UTC()
	reportPath := report.Filename(cfg.ReportsDir, runStart)

	directive, err := os.ReadFile(cfg.Directive)
	if err != nil {
		return "", fmt.Errorf("read directive: %w", err)
	}

	bridge, err := mcpbridge.New(ctx, cfg.GeMcpPath, cfg.GeMcpDSN)
	if err != nil {
		return "", err
	}
	defer bridge.Close()

	mcpTools, err := bridge.Tools(ctx)
	if err != nil {
		return "", err
	}
	log.Printf("ge-mcp up: %d tools", len(mcpTools))

	tools := make([]json.RawMessage, 0, len(mcpTools)+1)
	for _, t := range mcpTools {
		raw, err := json.Marshal(t)
		if err != nil {
			return "", err
		}
		tools = append(tools, raw)
	}
	tools = append(tools, submitReportDef)

	client := &llm.Client{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Model,
		HTTP: newHTTPClient()}

	system := string(directive) + "\n\n---\n\n" + harnessPreamble(runStart)
	history := []llm.Message{{Role: "user", Content: llm.TextContent(
		"Run one full research cycle now, per the directive. Finish by calling submit_report with the complete report.")}}

	nudges := 0
	for turn := 1; turn <= cfg.MaxTurns; turn++ {
		resp, err := client.Send(ctx, system, history, tools, cfg.MaxTokens)
		if err != nil {
			return failRun(reportPath, bridge, fmt.Errorf("turn %d: %w", turn, err))
		}
		log.Printf("turn %d: stop=%s in=%d out=%d", turn, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
		history = append(history, llm.Message{Role: "assistant", Content: resp.Content})

		toolUses := collectToolUses(resp.Blocks)
		if len(toolUses) == 0 {
			// Model stopped talking instead of submitting. Nudge twice, then fail.
			if nudges++; nudges > 2 {
				return failRun(reportPath, bridge, fmt.Errorf("model ended without submit_report after %d nudges", nudges-1))
			}
			history = append(history, llm.Message{Role: "user", Content: llm.TextContent(
				"You have not submitted the report. Continue the cycle and finish by calling submit_report exactly once.")})
			continue
		}

		var results []any
		for _, tu := range toolUses {
			if tu.Name == submitReportTool {
				path, gateErr := handleSubmit(tu.Input, reportPath, bridge)
				if gateErr == "" {
					log.Printf("report accepted: %s", path)
					return path, nil
				}
				log.Printf("report rejected: %s", gateErr)
				results = append(results, llm.ToolResult(tu.ID, `{"error":{"code":"invalid_report","reason":"`+gateErr+`"}}`, true))
				continue
			}
			text, isErr, err := bridge.Call(ctx, tu.Name, tu.Input)
			if err != nil {
				return failRun(reportPath, bridge, fmt.Errorf("tool %s: %w", tu.Name, err))
			}
			log.Printf("  tool %s (err=%v, %d bytes)", tu.Name, isErr, len(text))
			results = append(results, llm.ToolResult(tu.ID, text, isErr))
		}
		history = append(history, llm.Message{Role: "user", Content: llm.MakeContent(results...)})
	}
	return failRun(reportPath, bridge, fmt.Errorf("MAX_TURNS (%d) exhausted without a valid report", cfg.MaxTurns))
}

// newHTTPClient allows long per-request times: M3 is a reasoning model and
// large tool-heavy turns can take minutes.
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}

func harnessPreamble(runStart time.Time) string {
	return fmt.Sprintf(`## Harness notes (from the runtime, not the directive)
- Current time: %s (UTC). Use this as the run timestamp in the report header.
- Do NOT write any file yourself. Finish by calling the submit_report tool exactly once
  with the complete markdown; the harness owns the filename and appends the
  authoritative tool-call appendix.
- Every tool response is a JSON envelope {as_of, data_window, row_count, rows, ...}.
  An empty rows with a note is a real "nothing traded" signal, not an error.`,
		runStart.Format("2006-01-02 15:04"))
}

func collectToolUses(blocks []llm.Block) []llm.Block {
	var out []llm.Block
	for _, b := range blocks {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

func handleSubmit(input json.RawMessage, reportPath string, bridge *mcpbridge.Bridge) (string, string) {
	var args struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Markdown) == "" {
		return "", "markdown must be a non-empty string"
	}
	if reason := report.Validate(args.Markdown); reason != "" {
		return "", reason
	}
	if err := report.Write(reportPath, args.Markdown, bridge.AuditLog()); err != nil {
		return "", "write failed: " + err.Error()
	}
	return reportPath, ""
}

// failRun preserves the audit trail: writes <name>-FAILED.md with the reason
// and every call made, then returns the error.
func failRun(reportPath string, bridge *mcpbridge.Bridge, cause error) (string, error) {
	failed := strings.TrimSuffix(reportPath, ".md") + "-FAILED.md"
	md := fmt.Sprintf("# FAILED run\n\nReason: %s\n\nNo valid report was submitted. The tool-call log below records everything this run actually did.\n", cause)
	if err := report.Write(failed, md, bridge.AuditLog()); err != nil {
		log.Printf("could not write failure report: %v", err)
	}
	return "", cause
}
