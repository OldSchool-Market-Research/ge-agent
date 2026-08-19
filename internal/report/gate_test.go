package report

import (
	"strings"
	"testing"
	"time"

	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
)

// A tool result containing a bare ``` sequence (e.g. an item name quoting
// markdown) must not be able to close the appendix's code fence early —
// that would let harness-witnessed data escape into arbitrary markdown in
// the "ground truth" audit section.
func TestAppendixEscapesFenceBreakingResult(t *testing.T) {
	calls := []mcpbridge.CallRecord{{
		Seq:    1,
		Tool:   "quote",
		Args:   []byte(`{"name":"x"}`),
		Result: "safe prefix\n```\ninjected heading\n# gotcha\n```\nsafe suffix",
		At:     time.Now(),
	}}
	out := appendix(calls)

	// The rendered result block must still be delimited by a single pair of
	// matching fences that are longer than any backtick run in the content,
	// so the embedded ``` cannot terminate the fence early.
	idx := strings.Index(out, "result:\n")
	if idx == -1 {
		t.Fatalf("appendix missing result block:\n%s", out)
	}
	block := out[idx+len("result:\n"):]
	delimEnd := strings.IndexByte(block, '\n')
	if delimEnd == -1 {
		t.Fatalf("could not find opening fence line:\n%s", block)
	}
	openFence := block[:delimEnd]
	if !strings.HasPrefix(openFence, "````") {
		t.Fatalf("expected fence longer than embedded ``` run, got %q", openFence)
	}
	if !strings.HasSuffix(openFence, "json") {
		t.Fatalf("expected json-tagged fence, got %q", openFence)
	}
	closing := strings.TrimSuffix(openFence, "json")
	// The content must appear intact between the opening and closing fence,
	// including the embedded ``` sequence, proving it was not treated as a
	// real fence boundary.
	rest := block[delimEnd+1:]
	closeIdx := strings.Index(rest, "\n"+closing)
	if closeIdx == -1 {
		t.Fatalf("could not find matching closing fence %q in:\n%s", closing, rest)
	}
	content := rest[:closeIdx]
	if !strings.Contains(content, "injected heading") || !strings.Contains(content, "```") {
		t.Fatalf("fence-breaking content was mangled instead of contained:\n%s", content)
	}
}

func TestFenceLen(t *testing.T) {
	cases := []struct {
		content string
		want    int
	}{
		{"plain text", 3},
		{"one ` backtick", 3},
		{"a ``` fence-breaking run", 4},
		{"nested ```` four-tick run", 5},
	}
	for _, c := range cases {
		if got := fenceLen(c.content); got != c.want {
			t.Errorf("fenceLen(%q) = %d, want %d", c.content, got, c.want)
		}
	}
}
