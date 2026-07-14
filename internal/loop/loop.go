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
	"github.com/osrs-ge/ge-agent/internal/strategy"
)

const submitReportTool = "submit_report"

var submitReportDef = json.RawMessage(`{
  "name": "submit_report",
  "description": "Submit the run's final markdown report plus the machine-readable strategy objects. Call exactly once, at the end. The harness validates the required sections (Header, Digest, Strategies, Proof, Discarded — in order) and the strategies array, writes the file with a UTC timestamp name, and appends the authoritative tool-call log. Do not include the appendix yourself.",
  "input_schema": {
    "type": "object",
    "properties": {
      "markdown": {"type": "string", "description": "The complete report markdown, all five sections in order"},
      "strategies": {
        "type": "array",
        "description": "Machine-readable strategy objects, one per shipped strategy — the authoritative copy downstream systems ingest (the markdown Strategies section is the human view; the two must agree). ALL gp/unit fields must be plain integers: no expressions, no commas, no units.",
        "items": {
          "type": "object",
          "properties": {
            "id": {"type": "string", "description": "<archetype>-<item-slug>-<yyyymmdd>, e.g. G-earth-battlestaff-20260714"},
            "archetype": {"type": "string", "enum": ["A","B","C","D","E","F","G"]},
            "title": {"type": "string"},
            "thesis": {"type": "string"},
            "items": {"type": "array", "items": {"type": "object", "properties": {
              "name": {"type": "string"}, "id": {"type": "integer"},
              "buy_limit": {"type": ["integer","null"]}, "members": {"type": ["boolean","null"]}
            }, "required": ["name","id"]}},
            "entry": {"type": "string", "description": "precise human rule"},
            "exit": {"type": "string", "description": "precise human rule"},
            "entry_price": {"type": "integer", "description": "the buy trigger in gp (plain integer)"},
            "exit_price": {"type": "integer", "description": "the sell/alch target in gp (plain integer)"},
            "kill_price": {"type": ["integer","null"], "description": "price of items[0] beyond which the strategy is dead; null if not price-defined"},
            "horizon": {"type": "string"},
            "capital_required": {"type": "integer", "description": "gp, plain integer"},
            "size": {"type": "object", "properties": {
              "buy_limit": {"type": "integer"}, "vol_constrained": {"type": "integer"}, "units_used": {"type": "integer"}
            }, "required": ["buy_limit","vol_constrained","units_used"]},
            "expected_value": {"type": "object", "properties": {
              "per_cycle_gp": {"type": "integer"}, "per_1h_gp": {"type": "integer", "description": "post-tax gp per hour (one full 4h buy-limit cycle ÷ 4)"},
              "per_day_gp": {"type": "integer"}, "roi_pct": {"type": "number"}
            }, "required": ["per_cycle_gp","per_1h_gp","per_day_gp","roi_pct"]},
            "confidence": {"type": "string", "enum": ["high","medium","low","insufficient_history"]},
            "confidence_why": {"type": "string"},
            "evidence": {"type": "string"},
            "invalidation": {"type": "string"},
            "risks": {"type": "array", "items": {"type": "string"}},
            "paper_trade": {"type": "string"}
          },
          "required": ["id","archetype","title","thesis","items","entry","exit","entry_price","exit_price","horizon","capital_required","size","expected_value","confidence","invalidation"]
        }
      }
    },
    "required": ["markdown", "strategies"]
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
	if cfg.BriefFile != "" {
		brief, err := os.ReadFile(cfg.BriefFile)
		if err != nil {
			return "", fmt.Errorf("read brief: %w", err)
		}
		system += "\n\n---\n\n## Run brief (from the orchestrator — constraints for THIS run; the directive still governs method)\n\n" + string(brief)
	}
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
				path, gateErr := handleSubmit(tu.Input, reportPath, runStart, bridge)
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

func handleSubmit(input json.RawMessage, reportPath string, runStart time.Time, bridge *mcpbridge.Bridge) (string, string) {
	var args struct {
		Markdown   string          `json:"markdown"`
		Strategies json.RawMessage `json:"strategies"`
	}
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Markdown) == "" {
		return "", "markdown must be a non-empty string"
	}
	if reason := report.Validate(args.Markdown); reason != "" {
		return "", reason
	}
	strategies, reason := strategy.Parse(args.Strategies)
	if reason != "" {
		return "", reason
	}
	if err := report.Write(reportPath, args.Markdown, bridge.AuditLog()); err != nil {
		return "", "write failed: " + err.Error()
	}
	if err := report.WriteSidecar(reportPath, strategy.Sidecar{
		RunStartedAt: runStart, ReportPath: reportPath, Strategies: strategies,
	}); err != nil {
		return "", "sidecar write failed: " + err.Error()
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
