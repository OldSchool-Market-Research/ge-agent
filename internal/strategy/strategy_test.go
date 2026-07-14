package strategy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// One valid fixture per archetype; every test mutates a copy.
const validS = `{
  "id": "S-yew-logs-20260714", "archetype": "S",
  "title": "Yew logs weekend window", "thesis": "supply peaks weekday evenings, demand weekend",
  "items": [{"name": "Yew logs", "id": 1515, "buy_limit": 25000, "members": false}],
  "entry": "buy offers Tue 02:00-05:00 UTC", "exit": "sell offers Sat 18:00-21:00 UTC",
  "entry_price": 240, "exit_price": 265, "kill_price": 210,
  "horizon": "one weekly cycle", "capital_required": 6000000,
  "size": {"buy_limit": 25000, "vol_constrained": 20000, "units_used": 20000},
  "expected_value": {"per_cycle_gp": 300000, "per_1h_gp": 1785, "per_day_gp": 42857, "roi_pct": 5.0},
  "confidence": "medium", "confidence_why": "obs>=12 per pooled bucket", "evidence": "seasonal_scan + seasonality(how)",
  "invalidation": "amplitude below tax for 2 weeks", "risks": ["trend_confound"], "paper_trade": "2 cycles",
  "buy_window": {"from_how": 50, "to_how": 53}, "sell_window": {"from_how": 162, "to_how": 165}
}`

const validV = `{
  "id": "V-zulrah-scale-20260714", "archetype": "V",
  "title": "Zulrah scale supply shock", "thesis": "ban wave craters botted supply before price adjusts",
  "items": [{"name": "Zulrah's scales", "id": 12934, "buy_limit": 30000, "members": true}],
  "entry": "buy if volume z crosses", "exit": "sell into the repricing",
  "entry_price": 95, "exit_price": 120, "kill_price": 80,
  "horizon": "hours to days after trigger", "capital_required": 2850000,
  "size": {"buy_limit": 30000, "vol_constrained": 25000, "units_used": 25000},
  "expected_value": {"per_cycle_gp": 500000, "per_1h_gp": 20000, "per_day_gp": 480000, "roi_pct": 20.0},
  "confidence": "low", "confidence_why": "trigger-dependent", "evidence": "volume_zscore",
  "invalidation": "volume normalizes without price move", "risks": ["false_trigger"], "paper_trade": "await trigger",
  "trigger": {"metric": "volume_zscore", "threshold": 4.0, "direction": "below", "window": "1h"},
  "direction": "ride"
}`

const validC = `{
  "id": "C-prayer-potion-decant-20260714", "archetype": "C",
  "title": "Prayer potion 3->4 decant", "thesis": "dose-price divergence, direction-neutral",
  "items": [{"name": "Prayer potion(3)", "id": 139, "buy_limit": 2000, "members": true},
            {"name": "Prayer potion(4)", "id": 2434, "buy_limit": 2000, "members": true}],
  "entry": "buy 4x 3-dose at 6900", "exit": "sell 3x 4-dose at 9500",
  "entry_price": 27600, "exit_price": 27930, "kill_price": null,
  "horizon": "minutes per conversion", "capital_required": 13800000,
  "size": {"buy_limit": 500, "vol_constrained": 200, "units_used": 200},
  "expected_value": {"per_cycle_gp": 66000, "per_1h_gp": 16500, "per_day_gp": 396000, "roi_pct": 1.2},
  "confidence": "high", "confidence_why": "both legs fresh, deep volume", "evidence": "combo_quote relation 1",
  "invalidation": "combo margin negative for 1h", "risks": ["leg_fill_risk"], "paper_trade": "10 conversions",
  "legs": [{"item_id": 139, "name": "Prayer potion(3)", "side": "buy", "qty": 4, "price": 6900},
           {"item_id": 2434, "name": "Prayer potion(4)", "side": "sell", "qty": 3, "price": 9500}],
  "relation_id": 1
}`

const validU = `{
  "id": "U-dragon-bones-20260714", "archetype": "U",
  "title": "Update speculation: prayer training buff", "thesis": "announced prayer rework spikes bone demand",
  "items": [{"name": "Dragon bones", "id": 536, "buy_limit": 7500, "members": true}],
  "entry": "buy before Tuesday update", "exit": "sell into the spike",
  "entry_price": 2500, "exit_price": 3000, "kill_price": 2200,
  "horizon": "3 days around the update", "capital_required": 18750000,
  "size": {"buy_limit": 7500, "vol_constrained": 6000, "units_used": 6000},
  "expected_value": {"per_cycle_gp": 2400000, "per_1h_gp": 33000, "per_day_gp": 800000, "roi_pct": 16.0},
  "confidence": "low", "confidence_why": "event-conditional", "evidence": "movers + blog",
  "invalidation": "update ships without the buff", "risks": ["update_risk"], "paper_trade": "size half",
  "event": {"date": "2026-07-15", "description": "weekly game update, announced prayer changes"},
  "direction": "ride"
}`

const validH = `{
  "id": "H-rune-scimitar-20260714", "archetype": "H",
  "title": "Rune scimitar below 3-week band", "thesis": "temporary supply glut, stable demand floor",
  "items": [{"name": "Rune scimitar", "id": 1333, "buy_limit": 125, "members": false}],
  "entry": "buy at or below 14800", "exit": "sell at band median 15900",
  "entry_price": 14800, "exit_price": 15900, "kill_price": 13500,
  "horizon": "2-3 weeks", "capital_required": 1850000,
  "size": {"buy_limit": 125, "vol_constrained": 100, "units_used": 100},
  "expected_value": {"per_cycle_gp": 110000, "per_1h_gp": 327, "per_day_gp": 7857, "roi_pct": 7.4},
  "confidence": "medium", "confidence_why": "range_position 0.05 over 21d", "evidence": "screen range_position + item_history",
  "invalidation": "close below 13500", "risks": ["dead_capital"], "paper_trade": "one horizon",
  "eval_window_hours": 336
}`

var now = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

func parseOne(t *testing.T, fixture string) (map[string]any, func(map[string]any) string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fixture), &m); err != nil {
		t.Fatal(err)
	}
	rerun := func(m map[string]any) string {
		out, _ := json.Marshal([]any{m})
		var list []Strategy
		dec := json.NewDecoder(strings.NewReader(string(out)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&list); err != nil {
			return "decode: " + err.Error()
		}
		return Validate(list, now)
	}
	return m, rerun
}

func TestAllKindsValid(t *testing.T) {
	for name, fixture := range map[string]string{"S": validS, "V": validV, "C": validC, "U": validU, "H": validH} {
		t.Run(name, func(t *testing.T) {
			m, rerun := parseOne(t, fixture)
			if reason := rerun(m); reason != "" {
				t.Fatalf("expected valid %s, got: %s", name, reason)
			}
		})
	}
}

func TestGenericRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantSub string
	}{
		{"old archetype A", func(m map[string]any) { m["archetype"] = "A" }, "archetype"},
		{"old id letter", func(m map[string]any) { m["id"] = "A-yew-logs-20260714" }, "id"},
		{"bad id prefix", func(m map[string]any) { m["id"] = "V-yew-logs-20260714" }, "archetype letter"},
		{"bad id shape", func(m map[string]any) { m["id"] = "yew-logs" }, "id"},
		{"zero entry", func(m map[string]any) { m["entry_price"] = 0 }, "entry_price"},
		{"bad confidence", func(m map[string]any) { m["confidence"] = "certain" }, "confidence"},
		{"empty invalidation", func(m map[string]any) { m["invalidation"] = "" }, "invalidation"},
		{"eval window too long", func(m map[string]any) { m["eval_window_hours"] = 999 }, "eval_window_hours"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, rerun := parseOne(t, validS)
			c.mutate(m)
			reason := rerun(m)
			if reason == "" || !strings.Contains(reason, c.wantSub) {
				t.Fatalf("want rejection containing %q, got %q", c.wantSub, reason)
			}
		})
	}
}

func TestExpressionAsNumberRejected(t *testing.T) {
	bad := strings.Replace(validS, `"capital_required": 6000000`, `"capital_required": "6,000,000 gp"`, 1)
	_, reason := Parse(json.RawMessage("[" + bad + "]"))
	if reason == "" || !strings.Contains(reason, "plain integers") {
		t.Fatalf("want plain-integers rejection, got %q", reason)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	m, _ := map[string]any{}, 0
	_ = m
	bad := strings.Replace(validS, `"archetype": "S",`, `"archetype": "S", "made_up": 1,`, 1)
	_, reason := Parse(json.RawMessage("[" + bad + "]"))
	if reason == "" {
		t.Fatal("unknown field should be rejected")
	}
}

func TestSKindRules(t *testing.T) {
	t.Run("missing windows", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		delete(m, "buy_window")
		if reason := rerun(m); !strings.Contains(reason, "buy_window") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("bucket out of range", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		m["buy_window"] = map[string]any{"from_how": 170, "to_how": 3}
		if reason := rerun(m); !strings.Contains(reason, "0..167") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("overlapping windows", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		m["sell_window"] = map[string]any{"from_how": 52, "to_how": 60}
		if reason := rerun(m); !strings.Contains(reason, "overlap") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("wrap-around overlap detected", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		m["buy_window"] = map[string]any{"from_how": 165, "to_how": 2} // wraps over week end
		m["sell_window"] = map[string]any{"from_how": 0, "to_how": 5}  // shares 0-2
		if reason := rerun(m); !strings.Contains(reason, "overlap") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("wrap-around no overlap ok", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		m["buy_window"] = map[string]any{"from_how": 165, "to_how": 2}
		m["sell_window"] = map[string]any{"from_how": 80, "to_how": 85}
		if reason := rerun(m); reason != "" {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("entry must be below exit", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		m["entry_price"] = 300
		if reason := rerun(m); !strings.Contains(reason, "entry_price") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("short eval window rejected", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		m["eval_window_hours"] = 48
		if reason := rerun(m); !strings.Contains(reason, "168") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("cross-kind field rejected", func(t *testing.T) {
		m, rerun := parseOne(t, validS)
		m["trigger"] = map[string]any{"metric": "volume_zscore", "threshold": 3.0, "direction": "above", "window": "1h"}
		if reason := rerun(m); !strings.Contains(reason, "only valid for archetype V") {
			t.Fatalf("got %q", reason)
		}
	})
}

func TestVKindRules(t *testing.T) {
	t.Run("missing trigger", func(t *testing.T) {
		m, rerun := parseOne(t, validV)
		delete(m, "trigger")
		if reason := rerun(m); !strings.Contains(reason, "trigger") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("bad metric", func(t *testing.T) {
		m, rerun := parseOne(t, validV)
		m["trigger"] = map[string]any{"metric": "vibes", "threshold": 3.0, "direction": "above", "window": "1h"}
		if reason := rerun(m); !strings.Contains(reason, "metric") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("missing direction", func(t *testing.T) {
		m, rerun := parseOne(t, validV)
		delete(m, "direction")
		if reason := rerun(m); !strings.Contains(reason, "direction") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("missing kill price", func(t *testing.T) {
		m, rerun := parseOne(t, validV)
		m["kill_price"] = nil
		if reason := rerun(m); !strings.Contains(reason, "kill_price") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("no entry<exit ordering for fade", func(t *testing.T) {
		m, rerun := parseOne(t, validV)
		m["direction"] = "fade"
		m["entry_price"] = 120
		m["exit_price"] = 95 // fade: profit when price falls
		if reason := rerun(m); reason != "" {
			t.Fatalf("got %q", reason)
		}
	})
}

func TestCKindRules(t *testing.T) {
	t.Run("missing legs", func(t *testing.T) {
		m, rerun := parseOne(t, validC)
		delete(m, "legs")
		if reason := rerun(m); !strings.Contains(reason, "legs") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("all buy legs rejected", func(t *testing.T) {
		m, rerun := parseOne(t, validC)
		legs := m["legs"].([]any)
		legs[1].(map[string]any)["side"] = "buy"
		if reason := rerun(m); !strings.Contains(reason, "sell leg") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("leg item not in items", func(t *testing.T) {
		m, rerun := parseOne(t, validC)
		legs := m["legs"].([]any)
		legs[0].(map[string]any)["item_id"] = 99999
		if reason := rerun(m); !strings.Contains(reason, "must also appear in items") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("negative combo rejected", func(t *testing.T) {
		m, rerun := parseOne(t, validC)
		m["entry_price"] = 28000 // above exit 27930
		if reason := rerun(m); !strings.Contains(reason, "entry_price") {
			t.Fatalf("got %q", reason)
		}
	})
}

func TestUKindRules(t *testing.T) {
	t.Run("missing event", func(t *testing.T) {
		m, rerun := parseOne(t, validU)
		delete(m, "event")
		if reason := rerun(m); !strings.Contains(reason, "event") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("stale event", func(t *testing.T) {
		m, rerun := parseOne(t, validU)
		m["event"] = map[string]any{"date": "2026-06-01", "description": "old update"}
		if reason := rerun(m); !strings.Contains(reason, "within 14 days") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("bad date format", func(t *testing.T) {
		m, rerun := parseOne(t, validU)
		m["event"] = map[string]any{"date": "July 15th", "description": "update"}
		if reason := rerun(m); !strings.Contains(reason, "YYYY-MM-DD") {
			t.Fatalf("got %q", reason)
		}
	})
}

func TestHKindRules(t *testing.T) {
	t.Run("missing eval window", func(t *testing.T) {
		m, rerun := parseOne(t, validH)
		delete(m, "eval_window_hours")
		if reason := rerun(m); !strings.Contains(reason, "eval_window_hours") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("too short", func(t *testing.T) {
		m, rerun := parseOne(t, validH)
		m["eval_window_hours"] = 48
		if reason := rerun(m); !strings.Contains(reason, "168..672") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("missing kill price", func(t *testing.T) {
		m, rerun := parseOne(t, validH)
		m["kill_price"] = nil
		if reason := rerun(m); !strings.Contains(reason, "kill_price") {
			t.Fatalf("got %q", reason)
		}
	})
}

func TestSignalVerdicts(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		list, reason := ParseSignalVerdicts(json.RawMessage(
			`[{"signal_id": 3, "verdict": "shipped", "reason": "S strategy 1"},
			  {"signal_id": 7, "verdict": "dismissed", "reason": "stale leg"}]`))
		if reason != "" || len(list) != 2 {
			t.Fatalf("got %q, %d", reason, len(list))
		}
	})
	t.Run("absent ok", func(t *testing.T) {
		if list, reason := ParseSignalVerdicts(nil); reason != "" || list != nil {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("dismissed needs reason", func(t *testing.T) {
		_, reason := ParseSignalVerdicts(json.RawMessage(`[{"signal_id": 7, "verdict": "dismissed", "reason": ""}]`))
		if !strings.Contains(reason, "reason") {
			t.Fatalf("got %q", reason)
		}
	})
	t.Run("bad verdict", func(t *testing.T) {
		_, reason := ParseSignalVerdicts(json.RawMessage(`[{"signal_id": 7, "verdict": "maybe", "reason": "x"}]`))
		if !strings.Contains(reason, "verdict") {
			t.Fatalf("got %q", reason)
		}
	})
}

func TestEmptyAndTooMany(t *testing.T) {
	if _, reason := Parse(json.RawMessage(`[]`)); !strings.Contains(reason, "at least 1") {
		t.Fatalf("empty: %q", reason)
	}
}

func TestHourWindowContains(t *testing.T) {
	w := HourWindow{FromHow: 165, ToHow: 2}
	for _, b := range []int{165, 167, 0, 2} {
		if !w.Contains(b) {
			t.Fatalf("wrap window should contain %d", b)
		}
	}
	for _, b := range []int{3, 100, 164} {
		if w.Contains(b) {
			t.Fatalf("wrap window should not contain %d", b)
		}
	}
}
