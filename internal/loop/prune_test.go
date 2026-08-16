package loop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/osrs-ge/ge-agent/internal/llm"
)

func toolResultMsg(t *testing.T, id, content string) llm.Message {
	t.Helper()
	return llm.Message{Role: "user", Content: llm.MakeContent(llm.ToolResult(id, content, false))}
}

func assistantMsg(t *testing.T, blocksJSON string) llm.Message {
	t.Helper()
	return llm.Message{Role: "assistant", Content: json.RawMessage(blocksJSON)}
}

func bigResult() string {
	return `{"as_of":"2026-08-16T00:00:00Z","row_count":25,"rows":[` + strings.Repeat(`{"x":1},`, 400) + `{"x":1}]}`
}

func contentOf(t *testing.T, m llm.Message) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	return blocks
}

func TestPruneKeepsNewestTurnsIntact(t *testing.T) {
	big := bigResult()
	history := []llm.Message{
		{Role: "user", Content: llm.TextContent("go")},
		assistantMsg(t, `[{"type":"tool_use","id":"a","name":"quote","input":{}}]`),
		toolResultMsg(t, "a", big),
		assistantMsg(t, `[{"type":"tool_use","id":"b","name":"quote","input":{}}]`),
		toolResultMsg(t, "b", big),
		assistantMsg(t, `[{"type":"tool_use","id":"c","name":"quote","input":{}}]`),
		toolResultMsg(t, "c", big),
	}
	out, pruned := pruneHistory(history, 2)
	if pruned == 0 {
		t.Fatal("expected bytes pruned")
	}
	// Oldest result stubbed, newest two intact.
	first := contentOf(t, out[2])[0]["content"].(string)
	if len(first) >= len(big) || !strings.Contains(first, "pruned") {
		t.Fatalf("oldest result not stubbed: %d bytes", len(first))
	}
	for _, i := range []int{4, 6} {
		c := contentOf(t, out[i])[0]["content"].(string)
		if c != big {
			t.Fatalf("newest result at %d was modified", i)
		}
	}
	// Canonical history untouched.
	c := contentOf(t, history[2])[0]["content"].(string)
	if c != big {
		t.Fatal("pruning mutated the canonical history")
	}
	// tool_use_id survives the stubbing.
	if contentOf(t, out[2])[0]["tool_use_id"] != "a" {
		t.Fatal("tool_use_id lost in pruning")
	}
}

func TestPruneIsDeterministic(t *testing.T) {
	history := []llm.Message{
		toolResultMsg(t, "a", bigResult()),
		toolResultMsg(t, "b", bigResult()),
		toolResultMsg(t, "c", bigResult()),
	}
	out1, _ := pruneHistory(history, 1)
	out2, _ := pruneHistory(history, 1)
	for i := range out1 {
		if string(out1[i].Content) != string(out2[i].Content) {
			t.Fatalf("prune not deterministic at message %d", i)
		}
	}
}

func TestPruneSmallResultsUntouched(t *testing.T) {
	small := `{"as_of":"2026-08-16T00:00:00Z","row_count":0,"rows":[],"note":"nothing traded"}`
	history := []llm.Message{
		toolResultMsg(t, "a", small),
		toolResultMsg(t, "b", bigResult()),
		toolResultMsg(t, "c", bigResult()),
	}
	out, _ := pruneHistory(history, 1)
	if c := contentOf(t, out[0])[0]["content"].(string); c != small {
		t.Fatal("small result should never be pruned — null/empty results are signal")
	}
}

func TestPruneDisabled(t *testing.T) {
	history := []llm.Message{
		toolResultMsg(t, "a", bigResult()),
		toolResultMsg(t, "b", bigResult()),
	}
	out, _ := pruneHistory(history, 0)
	for i := range out {
		c := contentOf(t, out[i])[0]["content"].(string)
		if c != bigResult() {
			t.Fatalf("keepFullTurns<=0 must disable tool-result pruning (msg %d)", i)
		}
	}
}

func TestDropReasoningKeepsLastAssistant(t *testing.T) {
	thinking := `{"type":"thinking","thinking":"` + strings.Repeat("hmm ", 200) + `"}`
	history := []llm.Message{
		assistantMsg(t, `[`+thinking+`,{"type":"text","text":"first"}]`),
		{Role: "user", Content: llm.TextContent("continue")},
		assistantMsg(t, `[`+thinking+`,{"type":"tool_use","id":"z","name":"quote","input":{}}]`),
	}
	out, pruned := pruneHistory(history, 2)
	if pruned == 0 {
		t.Fatal("expected reasoning bytes dropped from the older assistant message")
	}
	if got := contentOf(t, out[0]); len(got) != 1 || got[0]["type"] != "text" {
		t.Fatalf("older assistant should keep only non-reasoning blocks, got %v", got)
	}
	// Last assistant message must stay byte-identical (tool-use contract).
	if string(out[2].Content) != string(history[2].Content) {
		t.Fatal("last assistant message must not be modified")
	}
}

func TestDropReasoningNeverEmptiesMessage(t *testing.T) {
	history := []llm.Message{
		assistantMsg(t, `[{"type":"thinking","thinking":"only thinking here"}]`),
		{Role: "user", Content: llm.TextContent("x")},
		assistantMsg(t, `[{"type":"text","text":"tail"}]`),
	}
	out, _ := pruneHistory(history, 2)
	if len(contentOf(t, out[0])) == 0 {
		t.Fatal("a message must never be emptied by reasoning-drop")
	}
	if string(out[0].Content) != string(history[0].Content) {
		t.Fatal("all-reasoning message should be kept intact rather than emptied")
	}
}
