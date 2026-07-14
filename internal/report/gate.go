// Package report enforces the DIRECTIVE's output contract mechanically:
// required sections in order, one file per run (O_EXCL — never overwrite),
// harness-chosen UTC filename, and an auto-appended audit appendix of every
// tool call the harness actually witnessed.
package report

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
)

// requiredSections must appear as markdown headings, in this order
// (DIRECTIVE §Output). Matched loosely: any heading level, case-insensitive.
var requiredSections = []string{"header", "digest", "strateg", "proof", "discard"}

var headingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)

// Validate checks the section contract. Returns a reason string usable as a
// typed tool error (empty = valid).
func Validate(markdown string) string {
	var headings []string
	for _, m := range headingRe.FindAllStringSubmatch(markdown, -1) {
		headings = append(headings, strings.ToLower(m[1]))
	}
	idx := 0
	for _, h := range headings {
		if idx < len(requiredSections) && strings.Contains(h, requiredSections[idx]) {
			idx++
		}
	}
	if idx < len(requiredSections) {
		return fmt.Sprintf("report is missing required section %q (need headings containing, in order: header, digest, strategies, proof, discarded)", requiredSections[idx])
	}
	return ""
}

// Filename returns the harness-chosen report path for this run (UTC).
func Filename(dir string, at time.Time) string {
	return filepath.Join(dir, at.UTC().Format("20060102-1504")+".md")
}

// Write persists the report plus the audit appendix. O_EXCL: a run never
// overwrites an existing report.
func Write(path, markdown string, calls []mcpbridge.CallRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(markdown); err != nil {
		return err
	}
	_, err = f.WriteString(appendix(calls))
	return err
}

// appendix renders the harness-witnessed tool-call log. This is the
// authoritative record "cite only real tool output" is audited against:
// a skeptic diffs the Proof section against these calls.
func appendix(calls []mcpbridge.CallRecord) string {
	var b strings.Builder
	b.WriteString("\n\n---\n\n## Appendix: harness tool-call log (auto-generated)\n\n")
	b.WriteString("Every ge-mcp call made this run, recorded by the harness — not the model. ")
	b.WriteString("Numbers in the Proof section that do not appear in a result below were not returned by any tool.\n\n")
	for _, c := range calls {
		fmt.Fprintf(&b, "### call %d — `%s` (%s, %s)\n\n", c.Seq, c.Tool, c.At.Format(time.RFC3339), c.Duration)
		fmt.Fprintf(&b, "params:\n```json\n%s\n```\n", string(c.Args))
		label := "result"
		if c.IsError {
			label = "result (tool error)"
		}
		fmt.Fprintf(&b, "%s:\n```json\n%s\n```\n\n", label, truncate(c.Result, 4000))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n… (truncated, %d bytes total)", len(s))
}
