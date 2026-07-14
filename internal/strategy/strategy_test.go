package strategy

import (
	"encoding/json"
	"strings"
	"testing"
)

const valid = `[{
  "id": "G-earth-battlestaff-20260714", "archetype": "G",
  "title": "Earth battlestaff alch floor", "thesis": "alch floor",
  "items": [{"name": "Earth battlestaff", "id": 1399, "buy_limit": 18000, "members": true}],
  "entry": "insta-buy", "exit": "high-alch 9300",
  "entry_price": 8748, "exit_price": 9300, "kill_price": 9200,
  "horizon": "4h cycles", "capital_required": 157464000,
  "size": {"buy_limit": 18000, "vol_constrained": 35539, "units_used": 18000},
  "expected_value": {"per_cycle_gp": 7416000, "per_1h_gp": 1854000, "per_day_gp": 29664000, "roi_pct": 4.71},
  "confidence": "high", "confidence_why": "n=25", "evidence": "calls 5,32",
  "invalidation": "nat > 250", "risks": ["fill_risk"], "paper_trade": "2 cycles"
}]`

func TestParseValid(t *testing.T) {
	list, reason := Parse(json.RawMessage(valid))
	if reason != "" {
		t.Fatalf("expected valid, got: %s", reason)
	}
	if len(list) != 1 || list[0].EntryPrice != 8748 {
		t.Fatalf("bad parse: %+v", list)
	}
}

func mutate(t *testing.T, field string, value any) string {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(valid), &list); err != nil {
		t.Fatal(err)
	}
	list[0][field] = value
	out, _ := json.Marshal(list)
	_, reason := Parse(out)
	return reason
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		value    any
		wantSub  string
	}{
		{"expression as number", "capital_required", "157,464,000 gp (18,000 x 8,748)", "must be"},
		{"bad archetype", "archetype", "Z", "archetype"},
		{"bad id", "id", "earth-staff", "id"},
		{"zero entry price", "entry_price", 0, "entry_price"},
		{"bad confidence", "confidence", "very high", "confidence"},
		{"empty invalidation", "invalidation", "", "invalidation"},
		{"entry above exit for A", "archetype", "A", "entry_price"}, // 8748 < 9300 ok... see below
	}
	for _, c := range cases[:6] {
		t.Run(c.name, func(t *testing.T) {
			reason := mutate(t, c.field, c.value)
			if reason == "" || !strings.Contains(reason, c.wantSub) {
				t.Fatalf("want rejection containing %q, got %q", c.wantSub, reason)
			}
		})
	}
}

func TestEntryBelowExitForFlips(t *testing.T) {
	var list []map[string]any
	json.Unmarshal([]byte(valid), &list)
	list[0]["archetype"] = "A"
	list[0]["id"] = "A-earth-battlestaff-20260714"
	list[0]["entry_price"] = 9400 // above exit 9300
	out, _ := json.Marshal(list)
	_, reason := Parse(out)
	if !strings.Contains(reason, "entry_price") {
		t.Fatalf("want entry_price rejection, got %q", reason)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	var list []map[string]any
	json.Unmarshal([]byte(valid), &list)
	list[0]["made_up_field"] = 1
	out, _ := json.Marshal(list)
	_, reason := Parse(out)
	if reason == "" {
		t.Fatal("unknown field should be rejected")
	}
}

func TestEmptyAndTooMany(t *testing.T) {
	if _, reason := Parse(json.RawMessage(`[]`)); !strings.Contains(reason, "at least 1") {
		t.Fatalf("empty: %q", reason)
	}
}
