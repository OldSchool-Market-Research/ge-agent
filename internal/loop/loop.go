// Package loop is the directive loop: DIRECTIVE.md as the system prompt,
// ge-mcp tools + the loop-local submit_report tool, run until the model
// submits a valid report (or the turn/time budget runs out — in which case a
// FAILED report with the audit log is written, so no run is silently lost).
package loop

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/osrs-ge/ge-agent/internal/config"
	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
	"github.com/osrs-ge/ge-agent/internal/report"
	"github.com/osrs-ge/ge-agent/internal/strategy"
)

const submitReportTool = "submit_report"

var submitReportDef = json.RawMessage(`{
  "name": "submit_report",
  "description": "Submit the run's final markdown report plus the machine-readable strategy objects. Call exactly once, at the end. The harness validates the required sections (Header, Digest, Strategies, Proof, Discarded — in order; the Processing watch section after them is not validated), the strategies array, and signal_verdicts, writes the file with a UTC timestamp name, and appends the authoritative tool-call log. Do not include the appendix yourself.",
  "input_schema": {
    "type": "object",
    "properties": {
      "markdown": {"type": "string", "description": "The complete report markdown: Header, Digest, Strategies, Proof, Discarded (validated, in order), then the Processing watch section"},
      "strategies": {
        "type": "array",
        "description": "Machine-readable strategy objects, one per shipped strategy — the authoritative copy downstream systems ingest (the markdown Strategies section is the human view; the two must agree). An EMPTY array is valid: ship nothing when nothing clears the bar (the Discarded section carries the evidence). ALL gp/unit fields must be plain integers: no expressions, no commas, no units. Kind-specific required fields: F needs attention + per_cycle_gp >= 400000; B needs attention+kill_price + entry_price >= 10000000; V needs trigger+direction+kill_price; C needs legs+relation_id; U needs event+direction+kill_price. S and H are retired — do not ship them. Fields belonging to another archetype are rejected.",
        "items": {
          "type": "object",
          "properties": {
            "id": {"type": "string", "description": "<archetype>-<item-slug>-<yyyymmdd>, e.g. F-adamantite-bar-20260720"},
            "archetype": {"type": "string", "enum": ["F","B","V","C","U"]},
            "title": {"type": "string"},
            "thesis": {"type": "string"},
            "items": {"type": "array", "items": {"type": "object", "properties": {
              "name": {"type": "string"}, "id": {"type": "integer"},
              "buy_limit": {"type": ["integer","null"]}, "members": {"type": ["boolean","null"]}
            }, "required": ["name","id"]}},
            "entry": {"type": "string", "description": "precise human rule"},
            "exit": {"type": "string", "description": "precise human rule"},
            "entry_price": {"type": "integer", "description": "the buy trigger in gp (plain integer). For C: total input cost per conversion"},
            "exit_price": {"type": "integer", "description": "the sell target in gp (plain integer). For C: post-tax output revenue per conversion"},
            "kill_price": {"type": ["integer","null"], "description": "price of items[0] beyond which the strategy is dead; null only where the archetype allows (required for B, V, U)"},
            "horizon": {"type": "string", "description": "expected hold / cycle time in words (B: the turnaround estimate)"},
            "attention": {"type": "string", "description": "REQUIRED for F, B, C: the execution contract — offer cadence, longest safe unattended window, reaction risk. The operator decides what fits their day"},
            "attention_spec": {"type": ["object","null"], "description": "REQUIRED for F, B, C: the attention contract as two JSON numbers (NOT quoted strings), agreeing with the attention prose", "properties": {
              "checks_per_hour": {"type": "number", "description": "GE visits per hour while running, > 0 and <= 12; fractional means less than hourly (0.25 = one check every 4h). Write 0.5, not \"0.5\""},
              "max_unattended_hours": {"type": "number", "description": "longest safe unattended window in hours, > 0 and <= 168"}
            }, "required": ["checks_per_hour","max_unattended_hours"]},
            "capital_required": {"type": "integer", "description": "gp, plain integer; must fit the 50M research budget on its own (per-opportunity, not a shared pool)"},
            "size": {"type": "object", "properties": {
              "buy_limit": {"type": "integer"}, "vol_constrained": {"type": "integer"}, "units_used": {"type": "integer"}
            }, "required": ["buy_limit","vol_constrained","units_used"]},
            "expected_value": {"type": "object", "properties": {
              "per_cycle_gp": {"type": "integer", "description": "post-tax; F: one 4h buy-limit cycle, MUST be >= 400000; B: MUST be >= 100000"}, "per_1h_gp": {"type": "integer", "description": "post-tax gp per hour on the archetype's own cycle (F: per_cycle/4; B: per_cycle/turnaround hours; C: one 4h buy-limit cycle / 4)"},
              "per_day_gp": {"type": "integer"}, "roi_pct": {"type": "number", "description": "display-only context — ranking is by absolute gp"}
            }, "required": ["per_cycle_gp","per_1h_gp","per_day_gp","roi_pct"]},
            "confidence": {"type": "string", "enum": ["high","medium","low","insufficient_history"]},
            "confidence_why": {"type": "string"},
            "evidence": {"type": "string"},
            "invalidation": {"type": "string"},
            "risks": {"type": "array", "items": {"type": "string"}},
            "paper_trade": {"type": "string"},
            "buy_window": {"type": ["object","null"], "description": "S ONLY, required for S: UTC hour-of-week range, from_how/to_how 0-167 (dow*24+hour, dow 0=Sunday), inclusive, from>to wraps", "properties": {
              "from_how": {"type": "integer"}, "to_how": {"type": "integer"}}, "required": ["from_how","to_how"]},
            "sell_window": {"type": ["object","null"], "description": "S ONLY, required for S: must not overlap buy_window", "properties": {
              "from_how": {"type": "integer"}, "to_how": {"type": "integer"}}, "required": ["from_how","to_how"]},
            "trigger": {"type": ["object","null"], "description": "V ONLY, required for V: the strategy ships ARMED and starts paper-trading when this fires", "properties": {
              "metric": {"type": "string", "enum": ["volume_zscore","price_move_pct"]},
              "threshold": {"type": "number"},
              "direction": {"type": "string", "enum": ["above","below"]},
              "window": {"type": "string", "description": "metric window, e.g. 1h"}
            }, "required": ["metric","threshold","direction","window"]},
            "direction": {"type": ["string","null"], "enum": ["ride","fade",null], "description": "V and U ONLY, required for both: ride the move or fade it"},
            "legs": {"type": "array", "description": "C ONLY, required for C: one row per conversion leg, numbers copied from combo_quote", "items": {"type": "object", "properties": {
              "item_id": {"type": "integer"}, "name": {"type": "string"},
              "side": {"type": "string", "enum": ["buy","sell"]},
              "qty": {"type": "integer", "description": "units per conversion"},
              "price": {"type": "integer", "description": "gp per unit"}
            }, "required": ["item_id","name","side","qty","price"]}},
            "relation_id": {"type": ["integer","null"], "description": "C ONLY, required for C: the item_relations row from list_relations"},
            "event": {"type": ["object","null"], "description": "U ONLY, required for U: the game event this trades (date within ±14 days)", "properties": {
              "date": {"type": "string", "description": "YYYY-MM-DD UTC"}, "description": {"type": "string"}
            }, "required": ["date","description"]},
            "eval_window_hours": {"type": ["integer","null"], "description": "How long the harness paper-trades before confirm/expire. Optional (defaults: F 48, B 96, V 96 from trigger, C 48, U 72)"}
          },
          "required": ["id","archetype","title","thesis","items","entry","exit","entry_price","exit_price","horizon","capital_required","size","expected_value","confidence","invalidation"]
        }
      },
      "signal_verdicts": {
        "type": "array",
        "description": "One verdict per ASSIGNED signal from the run brief's 'Assigned candidates' section (omit if the brief assigned none). Every assigned signal must be either shipped (a strategy came from it) or dismissed with the falsification reason.",
        "items": {"type": "object", "properties": {
          "signal_id": {"type": "integer"},
          "verdict": {"type": "string", "enum": ["shipped","dismissed"]},
          "reason": {"type": "string", "description": "for shipped: which strategy id; for dismissed: what killed it"}
        }, "required": ["signal_id","verdict","reason"]}
      },
      "proposals": {
        "type": "array",
        "description": "Optional, at most 3. Something worth the operator's attention that you can't ship: a recipe missing from item_relations, a new lane, a method change, a tool you lack. Only for something new: the brief lists what's already on file. An operator decides; nothing here trades. Each object: kind (recipe, lane, method or tool); title (one line, under 120 characters); detail (what it is and why it would pay, under 1000 characters); evidence (the tool calls and numbers from this run behind it, under 1000 characters); recipe (kind recipe only: per one conversion, as item_relations would hold it, {inputs: [{item_id, qty}], outputs: [{item_id, qty}], reversible}, integers for item_id and qty). A malformed one is dropped and never costs you the report.",
        "items": {"type": "object"}
      }
    },
    "required": ["markdown", "strategies"]
  }
}`)

const (
	kickoffPrompt = "Run one full research cycle now, per the directive. Finish by calling submit_report with the complete report."
	nudgePrompt   = "You have not submitted the report. Continue the cycle and finish by calling submit_report exactly once."
	// A model that stops talking instead of submitting is nudged this many
	// times before the run fails.
	maxNudges = 2
	// A rejected submit_report means a complete, fixable report is in hand —
	// the highest-value turns of the whole run. Grant a one-time grace
	// extension the first time a submit is rejected, so a rejection landing
	// near the turn budget doesn't throw away the entire cycle's research.
	graceTurns = 4
	// The same rejection three times running means the model is not acting
	// on the hint — run 1183 (2026-09-11) resubmitted one malformed field 40
	// times and burned the grace plus half the budget on it. Stop paying.
	maxSameRejections = 3
)

// systemPrompt is DIRECTIVE.md, the driver's harness notes, then the run
// brief when the orchestrator wrote one.
func systemPrompt(cfg *config.Config, preamble string) (string, error) {
	directive, err := os.ReadFile(cfg.Directive)
	if err != nil {
		return "", fmt.Errorf("read directive: %w", err)
	}
	system := string(directive) + "\n\n---\n\n" + preamble
	if cfg.BriefFile != "" {
		brief, err := os.ReadFile(cfg.BriefFile)
		if err != nil {
			return "", fmt.Errorf("read brief: %w", err)
		}
		system += "\n\n---\n\n## Run brief (from the orchestrator — constraints for THIS run; the directive still governs method)\n\n" + string(brief)
	}
	return system, nil
}

// rejectionPayload is the tool_result body for a rejected submit_report:
// the gate's reason plus a fix-only-the-field hint, escalated when the model
// repeats the same mistake.
func rejectionPayload(gateErr string, sameRejects int) string {
	hint := gateErr + " -- fix ONLY the named field and resubmit; do not re-run research"
	if sameRejects > 1 {
		hint = "SAME REJECTION AS YOUR PREVIOUS ATTEMPT -- your fix did not take. Change the named field's value in the JSON you send, then resubmit. " + hint
	}
	payload, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "invalid_report", "reason": hint}})
	return string(payload)
}

// auditor is whatever witnessed the run's tool calls: the MCP proxy (which
// also replays the calls of an earlier proxy in the same run), or the
// driver's reader of the proxy's audit file.
type auditor interface {
	AuditLog() []mcpbridge.CallRecord
}

func handleSubmit(input json.RawMessage, reportPath string, runStart time.Time, audit auditor) (string, string) {
	var args struct {
		Markdown       string          `json:"markdown"`
		Strategies     json.RawMessage `json:"strategies"`
		SignalVerdicts json.RawMessage `json:"signal_verdicts"`
		Proposals      json.RawMessage `json:"proposals"`
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
	verdicts, reason := strategy.ParseSignalVerdicts(args.SignalVerdicts)
	if reason != "" {
		return "", reason
	}
	proposals, dropped := strategy.ParseProposals(args.Proposals)
	if err := report.Write(reportPath, args.Markdown, audit.AuditLog()); err != nil {
		return "", "write failed: " + err.Error()
	}
	if err := report.WriteSidecar(reportPath, strategy.Sidecar{
		RunStartedAt: runStart, ReportPath: reportPath, Strategies: strategies,
		SignalVerdicts: verdicts, Proposals: proposals, ProposalsDropped: dropped,
	}); err != nil {
		return "", "sidecar write failed: " + err.Error()
	}
	return reportPath, ""
}

// failRun preserves the audit trail: writes <name>-FAILED.md with the reason
// and every call made, plus the run's cost record — a failed run burned
// tokens too, and that is exactly when the burn must stay visible.
func failRun(reportPath string, audit auditor, stats *report.RunStats, cause error) (string, error) {
	failed := strings.TrimSuffix(reportPath, ".md") + "-FAILED.md"
	md := fmt.Sprintf("# FAILED run\n\nReason: %s\n\nNo valid report was submitted. The tool-call log below records everything this run actually did.\n", cause)
	if err := report.Write(failed, md, audit.AuditLog()); err != nil {
		log.Printf("could not write failure report: %v", err)
	}
	stats.Outcome = "failed"
	stats.FailReason = cause.Error()
	if err := report.WriteStats(failed, *stats); err != nil {
		log.Printf("could not write run stats: %v", err)
	}
	return "", cause
}
