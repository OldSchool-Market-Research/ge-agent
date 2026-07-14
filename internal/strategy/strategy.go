// Package strategy defines the machine-readable strategy object the model
// must emit via submit_report, and its validation. Strict on purpose: the
// orchestrator's scoreboard is only as honest as these numbers, so
// expressions, comma-formatted strings, and unknown fields are rejected at
// the gate where the model can fix its own output.
package strategy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Item struct {
	Name     string `json:"name"`
	ID       int    `json:"id"`
	BuyLimit *int64 `json:"buy_limit"`
	Members  *bool  `json:"members"`
}

type Size struct {
	BuyLimit       int64 `json:"buy_limit"`
	VolConstrained int64 `json:"vol_constrained"`
	UnitsUsed      int64 `json:"units_used"`
}

type ExpectedValue struct {
	PerCycleGp int64   `json:"per_cycle_gp"`
	Per1hGp    int64   `json:"per_1h_gp"`
	PerDayGp   int64   `json:"per_day_gp"`
	RoiPct     float64 `json:"roi_pct"`
}

type Strategy struct {
	ID              string        `json:"id"`
	Archetype       string        `json:"archetype"`
	Title           string        `json:"title"`
	Thesis          string        `json:"thesis"`
	Items           []Item        `json:"items"`
	Entry           string        `json:"entry"`
	Exit            string        `json:"exit"`
	EntryPrice      int64         `json:"entry_price"`
	ExitPrice       int64         `json:"exit_price"`
	KillPrice       *int64        `json:"kill_price"`
	Horizon         string        `json:"horizon"`
	CapitalRequired int64         `json:"capital_required"`
	Size            Size          `json:"size"`
	ExpectedValue   ExpectedValue `json:"expected_value"`
	Confidence      string        `json:"confidence"`
	ConfidenceWhy   string        `json:"confidence_why"`
	Evidence        string        `json:"evidence"`
	Invalidation    string        `json:"invalidation"`
	Risks           []string      `json:"risks"`
	PaperTrade      string        `json:"paper_trade"`
}

// Sidecar is the machine-readable artifact written next to the report.
type Sidecar struct {
	RunStartedAt time.Time  `json:"run_started_at"`
	ReportPath   string     `json:"report_path"`
	Strategies   []Strategy `json:"strategies"`
}

var (
	idRe        = regexp.MustCompile(`^[A-F]-[a-z0-9-]+-\d{8}$`)
	archetypes  = map[string]bool{"A": true, "B": true, "C": true, "D": true, "E": true, "F": true}
	confidences = map[string]bool{"high": true, "medium": true, "low": true, "insufficient_history": true}
)

// Parse decodes the raw strategies array strictly (unknown fields and
// string-where-number both fail) and validates it. The returned string is a
// field-precise rejection reason for the model, "" when valid.
func Parse(raw json.RawMessage) ([]Strategy, string) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var list []Strategy
	if err := dec.Decode(&list); err != nil {
		var typeErr *json.UnmarshalTypeError
		if ok := isTypeError(err, &typeErr); ok {
			return nil, fmt.Sprintf("strategies: field %q must be %s, got %s — plain integers only, no expressions, no commas, no units", typeErr.Field, typeErr.Type, typeErr.Value)
		}
		return nil, "strategies: invalid JSON: " + err.Error()
	}
	if reason := Validate(list); reason != "" {
		return nil, reason
	}
	return list, ""
}

func isTypeError(err error, target **json.UnmarshalTypeError) bool {
	te, ok := err.(*json.UnmarshalTypeError)
	if ok {
		*target = te
	}
	return ok
}

// Validate applies the gate rules. Semantic plausibility (does per_1h ≈
// margin × units / 4) is deliberately NOT checked — that is the evaluator's job,
// and over-gating causes retry loops.
func Validate(list []Strategy) string {
	if len(list) == 0 {
		return "strategies: must contain at least 1 strategy"
	}
	if len(list) > 10 {
		return "strategies: at most 10 strategies"
	}
	for i, s := range list {
		p := func(field, problem string) string {
			return fmt.Sprintf("strategies[%d].%s: %s", i, field, problem)
		}
		switch {
		case !idRe.MatchString(s.ID):
			return p("id", `must match ^[A-F]-<item-slug>-<yyyymmdd>$ (e.g. "A-earth-battlestaff-20260714")`)
		case !archetypes[s.Archetype]:
			return p("archetype", "must be one of A-F")
		case strings.TrimSpace(s.Title) == "":
			return p("title", "required")
		case strings.TrimSpace(s.Thesis) == "":
			return p("thesis", "required")
		case len(s.Items) == 0:
			return p("items", "must contain at least one item")
		case strings.TrimSpace(s.Entry) == "":
			return p("entry", "required")
		case strings.TrimSpace(s.Exit) == "":
			return p("exit", "required")
		case s.EntryPrice <= 0:
			return p("entry_price", "must be a positive integer (gp)")
		case s.ExitPrice <= 0:
			return p("exit_price", "must be a positive integer (gp)")
		case s.KillPrice != nil && *s.KillPrice <= 0:
			return p("kill_price", "must be a positive integer (gp) or null")
		case strings.TrimSpace(s.Horizon) == "":
			return p("horizon", "required")
		case s.CapitalRequired < 0:
			return p("capital_required", "must be a non-negative integer (gp)")
		case s.Size.UnitsUsed <= 0:
			return p("size.units_used", "must be a positive integer")
		case !confidences[s.Confidence]:
			return p("confidence", "must be one of high | medium | low | insufficient_history")
		case strings.TrimSpace(s.Invalidation) == "":
			return p("invalidation", "required")
		}
		for j, it := range s.Items {
			if it.ID <= 0 {
				return fmt.Sprintf("strategies[%d].items[%d].id: must be a positive item_id", i, j)
			}
			if strings.TrimSpace(it.Name) == "" {
				return fmt.Sprintf("strategies[%d].items[%d].name: required", i, j)
			}
		}
		// Long spreads must buy below sell. Not enforced for D/E/F
		// (direction may be short / temporal).
		switch s.Archetype {
		case "A", "B", "C":
			if s.EntryPrice >= s.ExitPrice {
				return p("entry_price", fmt.Sprintf("must be below exit_price for archetype %s (buy low, sell high)", s.Archetype))
			}
		}
		if s.Size.BuyLimit > 0 && s.Size.VolConstrained > 0 &&
			s.Size.UnitsUsed > max64(s.Size.BuyLimit, s.Size.VolConstrained) {
			return p("size.units_used", "cannot exceed both buy_limit and vol_constrained")
		}
	}
	return ""
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
