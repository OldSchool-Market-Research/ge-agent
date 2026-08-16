// Transcript pruning: the loop resends the whole history every turn, so by
// the token teardown of run 921 (~21 turns) most of a run's input spend was
// old tool results and stale reasoning blocks being re-billed each turn.
// Before each Send the history is pruned on a COPY — the canonical history
// (and the audit log, and the report appendix) always keeps full results.
//
// Pruning is deterministic (same message always prunes to the same bytes) so
// the already-pruned prefix is byte-stable across turns — a provider that
// honors prompt caching can then reuse the prefix.
package loop

import (
	"encoding/json"
	"fmt"

	"github.com/osrs-ge/ge-agent/internal/llm"
)

const (
	// pruneMinBytes: tool_result contents at or below this size are never
	// pruned — the stub plus marker would not be meaningfully smaller.
	pruneMinBytes = 600
	// pruneKeepChars of the original content survive in the stub: enough for
	// the envelope's as_of / data_window / row_count head, not the rows.
	pruneKeepChars = 240
)

// reasoningTypes are content-block types that carry model reasoning. Older
// turns' reasoning has no forward value but full forward cost; only the
// latest assistant message keeps its blocks (the Anthropic tool-use contract
// requires exactly that one intact).
var reasoningTypes = map[string]bool{
	"thinking": true, "redacted_thinking": true, "reasoning": true,
}

// pruneHistory returns a pruned copy of history for sending: tool_result
// contents in all but the last keepFullTurns tool-result-bearing user
// messages are cut to a stub, and reasoning blocks are dropped from every
// assistant message except the last. Returns the copy and how many bytes the
// pruning removed this call. keepFullTurns <= 0 disables tool-result pruning
// (reasoning is still dropped — it is never re-read).
func pruneHistory(history []llm.Message, keepFullTurns int) ([]llm.Message, int) {
	out := make([]llm.Message, len(history))
	copy(out, history)

	lastAssistant := -1
	var resultMsgs []int
	for i, m := range out {
		switch m.Role {
		case "assistant":
			lastAssistant = i
		case "user":
			if hasBlockType(m.Content, "tool_result") {
				resultMsgs = append(resultMsgs, i)
			}
		}
	}

	pruned := 0
	if keepFullTurns > 0 && len(resultMsgs) > keepFullTurns {
		for _, i := range resultMsgs[:len(resultMsgs)-keepFullTurns] {
			c, n := pruneToolResults(out[i].Content)
			out[i] = llm.Message{Role: out[i].Role, Content: c}
			pruned += n
		}
	}
	for i, m := range out {
		if m.Role == "assistant" && i != lastAssistant {
			c, n := dropReasoning(m.Content)
			out[i] = llm.Message{Role: m.Role, Content: c}
			pruned += n
		}
	}
	return out, pruned
}

func hasBlockType(content json.RawMessage, typ string) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == typ {
			return true
		}
	}
	return false
}

// pruneToolResults cuts every oversized string tool_result content in the
// message down to a stub. On any parse surprise the message is returned
// unchanged — pruning is an optimization, never a correctness risk.
func pruneToolResults(content json.RawMessage) (json.RawMessage, int) {
	var blocks []map[string]any
	if json.Unmarshal(content, &blocks) != nil {
		return content, 0
	}
	pruned := 0
	for _, b := range blocks {
		if b["type"] != "tool_result" {
			continue
		}
		s, ok := b["content"].(string)
		if !ok || len(s) <= pruneMinBytes {
			continue
		}
		b["content"] = s[:pruneKeepChars] + fmt.Sprintf(
			"… [harness: %d bytes pruned from this old result to keep the context small — the numbers you already cited stand; re-call the tool if you need the full data again]", len(s)-pruneKeepChars)
		pruned += len(s) - len(b["content"].(string))
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return content, 0
	}
	return raw, pruned
}

// dropReasoning removes reasoning-type blocks from an assistant message. If
// filtering would leave the message empty (or anything fails to parse), the
// original is kept.
func dropReasoning(content json.RawMessage) (json.RawMessage, int) {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return content, 0
	}
	kept := make([]json.RawMessage, 0, len(blocks))
	dropped := 0
	for _, raw := range blocks {
		var t struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &t) == nil && reasoningTypes[t.Type] {
			dropped += len(raw)
			continue
		}
		kept = append(kept, raw)
	}
	if dropped == 0 || len(kept) == 0 {
		return content, 0
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return content, 0
	}
	return raw, dropped
}
