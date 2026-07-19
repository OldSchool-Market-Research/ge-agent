// Package strategy defines the machine-readable strategy object the model
// must emit via submit_report, and its validation. Strict on purpose: the
// orchestrator's scoreboard is only as honest as these numbers, so
// expressions, comma-formatted strings, and unknown fields are rejected at
// the gate where the model can fix its own output.
//
// Archetypes (2026-07-18 flips-first redesign): F volume flip, B high-value
// flip, V volume-anomaly (armed trigger), C conversion arbitrage (multi-leg),
// U update/event. S (seasonal window) and H (swing hold) are retired as
// shippable kinds — the letters stay valid so historical sidecars replay, but
// the directive forbids shipping them. Each kind has required structured
// fields the orchestrator's per-kind evaluator interprets; fields belonging
// to another kind are rejected so the DB rows stay clean.
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

// HourWindow is a UTC hour-of-week range, buckets 0-167 = dow*24 + hour,
// dow 0 = Sunday. from > to wraps across the week boundary (Sat night into
// Sunday). Both ends inclusive.
type HourWindow struct {
	FromHow int `json:"from_how"`
	ToHow   int `json:"to_how"`
}

// contains reports whether bucket b (0-167) falls inside the window,
// wrap-aware, both ends inclusive.
func (w HourWindow) Contains(b int) bool {
	if w.FromHow <= w.ToHow {
		return b >= w.FromHow && b <= w.ToHow
	}
	return b >= w.FromHow || b <= w.ToHow
}

// Trigger arms a V strategy: it starts paper-trading only when the metric
// crosses the threshold. The orchestrator evaluates it every tick with the
// same computation as ge-mcp's volume_zscore.
type Trigger struct {
	Metric    string  `json:"metric"`    // volume_zscore | price_move_pct
	Threshold float64 `json:"threshold"` // fire when metric crosses this
	Direction string  `json:"direction"` // above | below
	Window    string  `json:"window"`    // metric window, e.g. "1h"
}

// Leg is one side of a C conversion, priced per unit. Copy the numbers from
// combo_quote — the evaluator re-prices all legs each tick.
type Leg struct {
	ItemID int    `json:"item_id"`
	Name   string `json:"name"`
	Side   string `json:"side"` // buy | sell
	Qty    int64  `json:"qty"`  // units per conversion
	Price  int64  `json:"price"`
}

// Event anchors a U strategy to a real-world game event.
type Event struct {
	Date        string `json:"date"` // YYYY-MM-DD (UTC)
	Description string `json:"description"`
}

// SignalVerdict is the run's verdict on one assigned signal from the brief's
// work queue: every assigned signal must be either shipped (a strategy
// references it) or dismissed with the reason it failed falsification.
type SignalVerdict struct {
	SignalID int    `json:"signal_id"`
	Verdict  string `json:"verdict"` // shipped | dismissed
	Reason   string `json:"reason"`
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
	Attention       string        `json:"attention,omitempty"` // required for F, B: offer cadence, longest safe unattended window, reaction risk
	CapitalRequired int64         `json:"capital_required"`
	Size            Size          `json:"size"`
	ExpectedValue   ExpectedValue `json:"expected_value"`
	Confidence      string        `json:"confidence"`
	ConfidenceWhy   string        `json:"confidence_why"`
	Evidence        string        `json:"evidence"`
	Invalidation    string        `json:"invalidation"`
	Risks           []string      `json:"risks"`
	PaperTrade      string        `json:"paper_trade"`

	// Kind-specific structured fields (see Validate for the matrix).
	BuyWindow       *HourWindow `json:"buy_window,omitempty"`        // S
	SellWindow      *HourWindow `json:"sell_window,omitempty"`       // S
	Trigger         *Trigger    `json:"trigger,omitempty"`           // V
	Direction       *string     `json:"direction,omitempty"`         // V, U: ride | fade
	Legs            []Leg       `json:"legs,omitempty"`              // C
	RelationID      *int        `json:"relation_id,omitempty"`       // C
	Event           *Event      `json:"event,omitempty"`             // U
	EvalWindowHours *int        `json:"eval_window_hours,omitempty"` // required for H (168-672); optional elsewhere
}

// Sidecar is the machine-readable artifact written next to the report.
type Sidecar struct {
	RunStartedAt   time.Time       `json:"run_started_at"`
	ReportPath     string          `json:"report_path"`
	Strategies     []Strategy      `json:"strategies"`
	SignalVerdicts []SignalVerdict `json:"signal_verdicts,omitempty"`
}

var (
	idRe        = regexp.MustCompile(`^[FBSVCUH]-[a-z0-9-]+-\d{8}$`)
	archetypes  = map[string]bool{"F": true, "B": true, "S": true, "V": true, "C": true, "U": true, "H": true}
	confidences = map[string]bool{"high": true, "medium": true, "low": true, "insufficient_history": true}
	dateRe      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Flips-first constants (2026-07-18 redesign). The budget is a per-opportunity
// sizing scale, not a shared pool; the floors are absolute-gp ship gates.
const (
	ResearchBudgetGp = 50_000_000
	FloorFPerCycleGp = 200_000
	FloorBPerCycleGp = 100_000
	MinBEntryPriceGp = 10_000_000
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
	if reason := Validate(list, time.Now().UTC()); reason != "" {
		return nil, reason
	}
	return list, ""
}

// ParseSignalVerdicts decodes and validates the optional signal_verdicts
// array. Returns nil for absent/empty input.
func ParseSignalVerdicts(raw json.RawMessage) ([]SignalVerdict, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var list []SignalVerdict
	if err := dec.Decode(&list); err != nil {
		return nil, "signal_verdicts: invalid JSON: " + err.Error()
	}
	for i, v := range list {
		switch {
		case v.SignalID <= 0:
			return nil, fmt.Sprintf("signal_verdicts[%d].signal_id: must be a positive id from the brief", i)
		case v.Verdict != "shipped" && v.Verdict != "dismissed":
			return nil, fmt.Sprintf("signal_verdicts[%d].verdict: must be shipped or dismissed", i)
		case v.Verdict == "dismissed" && strings.TrimSpace(v.Reason) == "":
			return nil, fmt.Sprintf("signal_verdicts[%d].reason: required when dismissed — say what killed it", i)
		}
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
// margin × units) is deliberately NOT checked — that is the evaluator's job,
// and over-gating causes retry loops. now anchors the U event-date check.
func Validate(list []Strategy, now time.Time) string {
	// An empty list is valid: "nothing clears the bar" is a first-class
	// outcome — the Discarded section of the report carries the evidence.
	if len(list) > 10 {
		return "strategies: at most 10 strategies"
	}
	for i, s := range list {
		p := func(field, problem string) string {
			return fmt.Sprintf("strategies[%d].%s: %s", i, field, problem)
		}
		switch {
		case !idRe.MatchString(s.ID):
			return p("id", `must match ^[FBSVCUH]-<item-slug>-<yyyymmdd>$ (e.g. "F-adamantite-bar-20260720")`)
		case !archetypes[s.Archetype]:
			return p("archetype", "must be one of F | B | V | C | U (S and H are retired)")
		case !strings.HasPrefix(s.ID, s.Archetype+"-"):
			return p("id", "must start with the strategy's archetype letter")
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
		case s.CapitalRequired > ResearchBudgetGp:
			return p("capital_required", fmt.Sprintf("must fit the %dgp research budget on its own", ResearchBudgetGp))
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
		if s.Size.BuyLimit > 0 && s.Size.VolConstrained > 0 &&
			s.Size.UnitsUsed > max64(s.Size.BuyLimit, s.Size.VolConstrained) {
			return p("size.units_used", "cannot exceed both buy_limit and vol_constrained")
		}
		if s.EvalWindowHours != nil && (*s.EvalWindowHours < 1 || *s.EvalWindowHours > 672) {
			return p("eval_window_hours", "must be 1..672 (4 weeks max)")
		}

		// Per-kind matrix: required fields present, other kinds' fields absent.
		if reason := validateKind(i, s, now, p); reason != "" {
			return reason
		}
	}
	return ""
}

func validateKind(i int, s Strategy, now time.Time, p func(string, string) string) string {
	// Reject fields that belong to a different kind — the orchestrator's
	// per-kind evaluator would silently ignore them, which hides model
	// confusion. Precise beats permissive here.
	forbid := func(cond bool, field, kinds string) string {
		if cond {
			return p(field, "only valid for archetype "+kinds)
		}
		return ""
	}
	switch s.Archetype {
	case "F":
		if s.EntryPrice >= s.ExitPrice {
			return p("entry_price", "must be below exit_price for archetype F (the buy offer and the sell offer)")
		}
		if strings.TrimSpace(s.Attention) == "" {
			return p("attention", "required for archetype F — offer cadence, longest safe unattended window, reaction risk")
		}
		if s.ExpectedValue.PerCycleGp < FloorFPerCycleGp {
			return p("expected_value.per_cycle_gp", fmt.Sprintf("must be >= %d for archetype F — below the floor does not ship, dismiss it instead", FloorFPerCycleGp))
		}
		for _, f := range []string{
			forbid(s.BuyWindow != nil || s.SellWindow != nil, "buy_window/sell_window", "S"),
			forbid(s.Trigger != nil, "trigger", "V"),
			forbid(s.Direction != nil, "direction", "V/U"),
			forbid(len(s.Legs) > 0, "legs", "C"),
			forbid(s.RelationID != nil, "relation_id", "C"),
			forbid(s.Event != nil, "event", "U"),
		} {
			if f != "" {
				return f
			}
		}
	case "B":
		if s.EntryPrice < MinBEntryPriceGp {
			return p("entry_price", fmt.Sprintf("must be >= %d for archetype B — high-value flips are the 10M+ tier; cheaper items belong in F or nowhere", MinBEntryPriceGp))
		}
		if s.EntryPrice >= s.ExitPrice {
			return p("entry_price", "must be below exit_price for archetype B (buy offer below sell offer)")
		}
		if s.KillPrice == nil {
			return p("kill_price", "required for archetype B — a big-ticket hold without a stop is capital in a hole")
		}
		if strings.TrimSpace(s.Attention) == "" {
			return p("attention", "required for archetype B — offer cadence, longest safe unattended window, reaction risk")
		}
		if s.ExpectedValue.PerCycleGp < FloorBPerCycleGp {
			return p("expected_value.per_cycle_gp", fmt.Sprintf("must be >= %d for archetype B — below the floor does not ship, dismiss it instead", FloorBPerCycleGp))
		}
		for _, f := range []string{
			forbid(s.BuyWindow != nil || s.SellWindow != nil, "buy_window/sell_window", "S"),
			forbid(s.Trigger != nil, "trigger", "V"),
			forbid(s.Direction != nil, "direction", "V/U"),
			forbid(len(s.Legs) > 0, "legs", "C"),
			forbid(s.RelationID != nil, "relation_id", "C"),
			forbid(s.Event != nil, "event", "U"),
		} {
			if f != "" {
				return f
			}
		}
	case "S":
		if s.BuyWindow == nil || s.SellWindow == nil {
			return p("buy_window/sell_window", "required for archetype S (UTC hour-of-week ranges, from_how/to_how 0-167 = dow*24+hour, dow 0=Sunday)")
		}
		for name, w := range map[string]*HourWindow{"buy_window": s.BuyWindow, "sell_window": s.SellWindow} {
			if w.FromHow < 0 || w.FromHow > 167 || w.ToHow < 0 || w.ToHow > 167 {
				return p(name, "from_how/to_how must be 0..167")
			}
		}
		if windowsOverlap(*s.BuyWindow, *s.SellWindow) {
			return p("sell_window", "must not overlap buy_window — the strategy is buy in one window, sell in the other")
		}
		if s.EntryPrice >= s.ExitPrice {
			return p("entry_price", "must be below exit_price for archetype S (buy the cheap window, sell the dear one)")
		}
		if s.EvalWindowHours != nil && *s.EvalWindowHours < 168 {
			return p("eval_window_hours", "must be >= 168 for archetype S (at least one full weekly cycle)")
		}
		for _, f := range []string{
			forbid(s.Trigger != nil, "trigger", "V"),
			forbid(s.Direction != nil, "direction", "V/U"),
			forbid(len(s.Legs) > 0, "legs", "C"),
			forbid(s.RelationID != nil, "relation_id", "C"),
			forbid(s.Event != nil, "event", "U"),
		} {
			if f != "" {
				return f
			}
		}
	case "V":
		if s.Trigger == nil {
			return p("trigger", "required for archetype V — the strategy ships ARMED and only trades when the trigger fires")
		}
		if s.Trigger.Metric != "volume_zscore" && s.Trigger.Metric != "price_move_pct" {
			return p("trigger.metric", "must be volume_zscore or price_move_pct")
		}
		if s.Trigger.Direction != "above" && s.Trigger.Direction != "below" {
			return p("trigger.direction", "must be above or below")
		}
		if s.Trigger.Metric == "volume_zscore" && s.Trigger.Threshold <= 0 {
			return p("trigger.threshold", "must be positive for volume_zscore")
		}
		if strings.TrimSpace(s.Trigger.Window) == "" {
			return p("trigger.window", "required (e.g. \"1h\")")
		}
		if s.Direction == nil || (*s.Direction != "ride" && *s.Direction != "fade") {
			return p("direction", "required for archetype V: ride (follow the move) or fade (bet on reversal)")
		}
		if s.KillPrice == nil {
			return p("kill_price", "required for archetype V — a triggered anomaly trade without a stop is a prayer")
		}
		for _, f := range []string{
			forbid(s.BuyWindow != nil || s.SellWindow != nil, "buy_window/sell_window", "S"),
			forbid(len(s.Legs) > 0, "legs", "C"),
			forbid(s.RelationID != nil, "relation_id", "C"),
			forbid(s.Event != nil, "event", "U"),
		} {
			if f != "" {
				return f
			}
		}
	case "C":
		if len(s.Legs) == 0 || s.RelationID == nil {
			return p("legs/relation_id", "required for archetype C — copy the legs from combo_quote and cite its relation_id")
		}
		if *s.RelationID <= 0 {
			return p("relation_id", "must be a positive id from list_relations")
		}
		var buys, sells int
		itemIDs := map[int]bool{}
		for _, it := range s.Items {
			itemIDs[it.ID] = true
		}
		for j, l := range s.Legs {
			lp := func(field, problem string) string {
				return fmt.Sprintf("strategies[%d].legs[%d].%s: %s", i, j, field, problem)
			}
			switch {
			case l.Side != "buy" && l.Side != "sell":
				return lp("side", "must be buy or sell")
			case l.ItemID <= 0:
				return lp("item_id", "must be a positive item_id")
			case strings.TrimSpace(l.Name) == "":
				return lp("name", "required")
			case l.Qty <= 0:
				return lp("qty", "must be a positive integer")
			case l.Price <= 0:
				return lp("price", "must be a positive integer (gp)")
			}
			if !itemIDs[l.ItemID] {
				return lp("item_id", "every leg item must also appear in items")
			}
			if l.Side == "buy" {
				buys++
			} else {
				sells++
			}
		}
		if buys == 0 || sells == 0 {
			return p("legs", "must contain at least one buy leg and one sell leg")
		}
		if s.EntryPrice >= s.ExitPrice {
			return p("entry_price", "must be below exit_price for archetype C (entry_price = input cost per conversion, exit_price = post-tax output revenue)")
		}
		for _, f := range []string{
			forbid(s.BuyWindow != nil || s.SellWindow != nil, "buy_window/sell_window", "S"),
			forbid(s.Trigger != nil, "trigger", "V"),
			forbid(s.Direction != nil, "direction", "V/U"),
			forbid(s.Event != nil, "event", "U"),
		} {
			if f != "" {
				return f
			}
		}
	case "U":
		if s.Event == nil {
			return p("event", "required for archetype U — the date and description of the game event this trades")
		}
		if !dateRe.MatchString(s.Event.Date) {
			return p("event.date", "must be YYYY-MM-DD (UTC)")
		}
		eventDate, err := time.Parse("2006-01-02", s.Event.Date)
		if err != nil {
			return p("event.date", "must be a real date (YYYY-MM-DD)")
		}
		if d := eventDate.Sub(now); d > 14*24*time.Hour || d < -14*24*time.Hour {
			return p("event.date", "must be within 14 days of the run — stale or far-future events are not tradeable dislocations")
		}
		if strings.TrimSpace(s.Event.Description) == "" {
			return p("event.description", "required")
		}
		if s.Direction == nil || (*s.Direction != "ride" && *s.Direction != "fade") {
			return p("direction", "required for archetype U: ride or fade the dislocation")
		}
		if s.KillPrice == nil {
			return p("kill_price", "required for archetype U")
		}
		for _, f := range []string{
			forbid(s.BuyWindow != nil || s.SellWindow != nil, "buy_window/sell_window", "S"),
			forbid(s.Trigger != nil, "trigger", "V"),
			forbid(len(s.Legs) > 0, "legs", "C"),
			forbid(s.RelationID != nil, "relation_id", "C"),
		} {
			if f != "" {
				return f
			}
		}
	case "H":
		if s.EvalWindowHours == nil {
			return p("eval_window_hours", "required for archetype H (168-672: the hold horizon in hours)")
		}
		if *s.EvalWindowHours < 168 || *s.EvalWindowHours > 672 {
			return p("eval_window_hours", "must be 168..672 for archetype H (1-4 weeks)")
		}
		if s.KillPrice == nil {
			return p("kill_price", "required for archetype H — a multi-week hold without a stop is capital in a hole")
		}
		if s.EntryPrice >= s.ExitPrice {
			return p("entry_price", "must be below exit_price for archetype H (buy below the band, sell on reversion)")
		}
		for _, f := range []string{
			forbid(s.BuyWindow != nil || s.SellWindow != nil, "buy_window/sell_window", "S"),
			forbid(s.Trigger != nil, "trigger", "V"),
			forbid(s.Direction != nil, "direction", "V/U"),
			forbid(len(s.Legs) > 0, "legs", "C"),
			forbid(s.RelationID != nil, "relation_id", "C"),
			forbid(s.Event != nil, "event", "U"),
		} {
			if f != "" {
				return f
			}
		}
	}
	return ""
}

// windowsOverlap reports whether two wrap-aware hour-of-week windows share
// any bucket.
func windowsOverlap(a, b HourWindow) bool {
	for h := 0; h < 168; h++ {
		if a.Contains(h) && b.Contains(h) {
			return true
		}
	}
	return false
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
