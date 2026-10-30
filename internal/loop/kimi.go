package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osrs-ge/ge-agent/internal/config"
	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
	"github.com/osrs-ge/ge-agent/internal/report"
)

// RunKimi executes one directive cycle with the Kimi Code CLI as the model
// loop. Kimi's coding plan has no API access, only its CLI (logged in once
// by device-code OAuth), so instead of calling a model endpoint ge-agent:
//
//  1. writes a per-run kimi workspace: an agent file whose prompt is the
//     same directive + harness notes + brief the api driver sends, with a
//     tool allowlist of mcp__ge__* only (no shell, file or web tools), and an
//     mcp.json naming `ge-agent -mcp-proxy` (proxy.go) as the one MCP server;
//  2. runs `kimi -p` with stream-json output, logging turns and tool calls
//     and enforcing MAX_TURNS plus the one-time rejection grace;
//  3. nudges a model that stops without submitting by resuming its session,
//     like the api loop's nudges;
//  4. reads the outcome the proxy recorded and the token usage from kimi's
//     session log, and writes the same report + stats artifacts, so the
//     orchestrator sees the drivers differ only in cost.
//
// Validated 2026-10-30 on run 8669's brief: MiniMax hit MAX_TURNS 80 with no
// report; K3.1 Pro submitted a valid report on its first attempt in 11 turns.
func RunKimi(ctx context.Context, cfg *config.Config) (string, error) {
	runStart := time.Now().UTC()
	reportPath := report.Filename(cfg.ReportsDir, runStart)
	absReport, err := filepath.Abs(reportPath)
	if err != nil {
		return "", err
	}
	system, err := systemPrompt(cfg, kimiPreamble(runStart))
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate ge-agent for the MCP proxy: %w", err)
	}
	ws, err := filepath.Abs("kimi")
	if err != nil {
		return "", err
	}
	if err := writeKimiWorkspace(ws, system, self); err != nil {
		return "", fmt.Errorf("kimi workspace: %w", err)
	}
	h := proxyHandoff{Dir: ws, ReportPath: absReport, RunStart: runStart}

	st := newKimiStream(cfg.MaxTurns)
	res, cause := driveKimi(ctx, cfg, ws, h, st)

	stats := report.RunStats{RunStartedAt: runStart, Outcome: "failed"}
	audit, err := readAudit(h.auditPath())
	if err != nil {
		log.Printf("could not read the proxy's audit log: %v", err)
	}
	stats.ToolCalls = len(audit)
	if st.sessionID == "" {
		// Stopping kimi the moment the report is accepted also skips its
		// closing resume hint; the session is the one run in this workspace.
		st.sessionID = findKimiSession(kimiHome(cfg), ws)
	}
	usage, err := readKimiUsage(kimiHome(cfg), st.sessionID)
	if err != nil {
		log.Printf("could not read kimi token usage: %v", err)
	}
	applyKimiUsage(&stats, usage, st)

	if cause != nil {
		return failRun(reportPath, auditLog(audit), &stats, cause)
	}
	log.Printf("report accepted: %s", res.ReportPath)
	stats.Outcome = "submitted"
	if err := report.WriteStats(reportPath, stats); err != nil {
		log.Printf("could not write run stats: %v", err)
	}
	return reportPath, nil
}

// driveKimi runs the session, plus up to maxNudges resumes, until the proxy
// records an outcome or the run ends without one. A nil error means the
// report was accepted.
func driveKimi(ctx context.Context, cfg *config.Config, ws string, h proxyHandoff, st *kimiStream) (*proxyResult, error) {
	prompt := kickoffPrompt
	for nudges := 0; ; nudges++ {
		runErr := runKimiOnce(ctx, cfg, ws, h, st, prompt)
		var res *proxyResult
		if err := readJSON(h.resultPath(), &res); err != nil {
			return nil, fmt.Errorf("read the proxy's outcome: %w", err)
		}
		switch {
		case res != nil && res.Outcome == "submitted":
			return res, nil
		case res != nil:
			return nil, errors.New(res.FailReason)
		case st.capHit:
			return nil, fmt.Errorf("MAX_TURNS (%d, incl. any grace) exhausted without a valid report", st.maxTurns)
		case ctx.Err() != nil:
			return nil, fmt.Errorf("turn %d: %w", st.turns, ctx.Err())
		case runErr != nil:
			return nil, fmt.Errorf("kimi: %w", runErr)
		case st.sessionID == "":
			return nil, errors.New("kimi: ended without a session id to resume")
		case nudges >= maxNudges:
			return nil, fmt.Errorf("model ended without submit_report after %d nudges", nudges)
		}
		log.Printf("kimi ended without submitting; nudge %d of %d", nudges+1, maxNudges)
		prompt = nudgePrompt
	}
}

// runKimiOnce runs one kimi invocation (the kickoff, or a nudge resuming the
// session) to exit, feeding its stream to st. Hitting the turn cap kills it.
func runKimiOnce(ctx context.Context, cfg *config.Config, ws string, h proxyHandoff, st *kimiStream, prompt string) error {
	args := []string{"-p", prompt, "--skills-dir", filepath.Join(ws, "skills"), "--output-format", "stream-json"}
	if st.sessionID != "" {
		// The resumed session keeps its agent profile; kimi refuses
		// --agent-file together with --session.
		args = append(args, "--session", st.sessionID)
	} else {
		args = append(args, "--agent-file", filepath.Join(ws, kimiAgentFile))
	}
	if cfg.KimiModel != "" {
		args = append(args, "-m", cfg.KimiModel)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.KimiCmd, args...)
	cmd.Dir = ws
	cmd.Env = kimiEnv(cfg, ws, h)
	// Its own process group, so a kill also reaches the MCP proxy and the
	// ge-mcp it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cfg.KimiCmd, err)
	}
	var wg sync.WaitGroup
	var lastErrLine string
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			log.Printf("kimi: %s", sc.Text())
			if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "error:") {
				lastErrLine = line
			}
		}
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // a submit_report call carries the whole report
	for sc.Scan() {
		if st.feed(sc.Bytes()) {
			cancel()
		}
	}
	wg.Wait()
	err = cmd.Wait()
	if err != nil && lastErrLine != "" {
		// The run's fail reason is what the dashboard classifies: carry
		// kimi's own words (e.g. a missing login), not just the exit code.
		return fmt.Errorf("%w: %s", err, lastErrLine)
	}
	return err
}

// kimiEnv is ge-agent's environment for the kimi CLI, which its MCP servers
// (the proxy, and through it ge-mcp's DSN) inherit — so no secret is ever
// written into the workspace's mcp.json.
func kimiEnv(cfg *config.Config, ws string, h proxyHandoff) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIMAX_API_KEY=") {
			continue // the api driver's key has no business in kimi's environment
		}
		env = append(env, kv)
	}
	// The orchestrator spawns ge-agent with a minimal environment, and kimi
	// is a node script (#!/usr/bin/env node) that also needs a home.
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	if os.Getenv("HOME") == "" {
		home := cfg.KimiHome
		if home == "" {
			home = ws
		}
		env = append(env, "HOME="+home)
	}
	if cfg.KimiHome != "" {
		env = append(env, "KIMI_CODE_HOME="+cfg.KimiHome)
	}
	env = append(env,
		"KIMI_CODE_TRUST_WORKSPACE=1", // else kimi skips the workspace's mcp.json
		"KIMI_CODE_NO_AUTO_UPDATE=1",  // the image pins the CLI version
		"GE_AGENT_DRIVER=kimi",        // the proxy needs no API key
	)
	return append(env, h.env()...)
}

const (
	kimiAgentFile  = "ge-analyst.md"
	kimiToolPrefix = "mcp__ge__"
)

func writeKimiWorkspace(ws, system, self string) error {
	for _, d := range []string{filepath.Join(ws, ".kimi-code"), filepath.Join(ws, "skills")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	agent := "---\nname: ge-analyst\ndescription: One OSRS Grand Exchange research cycle, per the directive\n" +
		"tools: " + kimiToolPrefix + "*\n---\n\n" + system + "\n"
	if err := os.WriteFile(filepath.Join(ws, kimiAgentFile), []byte(agent), 0o644); err != nil {
		return err
	}
	mcpCfg, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
		"ge": map[string]any{"command": self, "args": []string{"-mcp-proxy"}},
	}}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ws, ".kimi-code", "mcp.json"), mcpCfg, 0o644)
}

func kimiPreamble(runStart time.Time) string {
	return fmt.Sprintf(`## Harness notes (from the runtime, not the directive)
- Current time: %s (UTC). Use this as the run timestamp in the report header.
- Your tools are the directive's ge-mcp tools, named mcp__ge__<tool>
  (mcp__ge__combo_screen is the directive's combo_screen), plus
  mcp__ge__submit_report. You have no shell, file or web tools.
- Do NOT write any file yourself. Finish by calling submit_report exactly once
  with the complete markdown; the harness owns the filename and appends the
  authoritative tool-call appendix.
- Every tool response is a JSON envelope {as_of, data_window, row_count, rows, ...}.
  An empty rows with a note is a real "nothing traded" signal, not an error.`,
		runStart.Format("2006-01-02 15:04"))
}

// kimiStream follows kimi's stream-json output across invocations: one line
// per assistant message (a turn, with its tool calls), per tool result, and
// a closing session.resume_hint.
type kimiStream struct {
	maxTurns  int
	turns     int
	turnCalls []int             // ge-mcp calls per turn (submit_report excluded, as in the api loop)
	turnTools []int             // every tool call per turn, submit_report included
	names     map[string]string // tool_call_id -> tool name, prefix stripped
	grace     bool
	capHit    bool
	finished  bool // the proxy accepted the report or failed the run
	sessionID string
}

func newKimiStream(maxTurns int) *kimiStream {
	return &kimiStream{maxTurns: maxTurns, names: map[string]string{}}
}

type kimiLine struct {
	Role       string          `json:"role"`
	Type       string          `json:"type"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
	SessionID string `json:"session_id"`
}

// feed consumes one stream-json line and reports whether kimi should be
// stopped: the turn budget is spent (like the api loop, the model gets
// maxTurns turns whose tool calls all run; asking for one more ends the run),
// or the proxy has settled the run and any further turn is wasted.
func (s *kimiStream) feed(line []byte) bool {
	var l kimiLine
	if err := json.Unmarshal(line, &l); err != nil {
		log.Printf("kimi: %s", line)
		return false
	}
	switch {
	case l.Role == "assistant":
		if s.capHit || s.finished {
			return true
		}
		if s.turns >= s.maxTurns {
			s.capHit = true
			log.Printf("MAX_TURNS (%d) reached: stopping kimi", s.maxTurns)
			return true
		}
		s.turns++
		calls := 0
		for _, tc := range l.ToolCalls {
			name := strings.TrimPrefix(tc.Function.Name, kimiToolPrefix)
			s.names[tc.ID] = name
			if name != submitReportTool {
				calls++
			}
		}
		s.turnCalls = append(s.turnCalls, calls)
		s.turnTools = append(s.turnTools, len(l.ToolCalls))
		log.Printf("kimi turn %d: %d tool call(s)", s.turns, len(l.ToolCalls))
	case l.Role == "tool":
		name := s.names[l.ToolCallID]
		text := contentText(l.Content)
		if strings.Contains(text, "FATAL: ") || (name == submitReportTool && strings.Contains(text, "Report accepted")) {
			s.finished = true
			return true
		}
		if name != submitReportTool {
			// The stream carries no error flag; ge-mcp's errors are an
			// {"error": ...} envelope (the proxy logs the exact flag).
			log.Printf("  tool %s (err=%v, %d bytes)", name, strings.HasPrefix(strings.TrimSpace(text), `{"error"`), len(text))
			break
		}
		if reason, ok := rejectionReason(text); ok {
			log.Printf("report rejected: %s", reason)
			if !s.grace {
				s.grace = true
				s.maxTurns += graceTurns
				log.Printf("grace: +%d turns to fix the rejected report (budget now %d)", graceTurns, s.maxTurns)
			}
		}
	case l.Type == "session.resume_hint":
		s.sessionID = l.SessionID
	}
	return false
}

// rejectionReason finds the proxy's rejection payload in a submit_report
// result, tolerating any wrapper text kimi adds around an error result.
func rejectionReason(text string) (string, bool) {
	i := strings.Index(text, `{"error":{"code":"invalid_report"`)
	if i < 0 {
		return "", false
	}
	var rej struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&rej); err != nil {
		return "", false
	}
	return rej.Error.Reason, true
}

// contentText reads a message's content, a string in kimi's stream (parts
// arrays are joined just in case).
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

// kimiUsage is one usage.record from kimi's session wire log: one model call.
type kimiUsage struct {
	InputOther         int `json:"inputOther"`
	Output             int `json:"output"`
	InputCacheRead     int `json:"inputCacheRead"`
	InputCacheCreation int `json:"inputCacheCreation"`
}

func kimiHome(cfg *config.Config) string {
	if cfg.KimiHome != "" {
		return cfg.KimiHome
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".kimi-code")
	}
	return ""
}

// findKimiSession returns the id of the newest kimi session whose working
// directory is ws (each run has its own workspace), or "".
func findKimiSession(home, ws string) string {
	states, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "session_*", "state.json"))
	var id string
	var newest int64
	for _, path := range states {
		var s struct {
			ID        string `json:"id"`
			Cwd       string `json:"cwd"`
			CreatedAt int64  `json:"createdAt"`
		}
		if readJSON(path, &s) == nil && s.Cwd == ws && s.CreatedAt >= newest {
			id, newest = s.ID, s.CreatedAt
		}
	}
	return id
}

// readKimiUsage reads the session's per-call usage from
// <KIMI_CODE_HOME>/sessions/<workspace>/<session>/agents/main/wire.jsonl.
func readKimiUsage(home, sessionID string) ([]kimiUsage, error) {
	if home == "" || sessionID == "" {
		return nil, errors.New("no kimi home or session id")
	}
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", sessionID, "agents", "main", "wire.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no wire log for %s under %s: %w", sessionID, home, fs.ErrNotExist)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []kimiUsage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var rec struct {
			Type  string    `json:"type"`
			Usage kimiUsage `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Type == "usage.record" {
			out = append(out, rec.Usage)
		}
	}
	return out, sc.Err()
}

// applyKimiUsage fills the cost record. InputTokens counts every input token
// (cache hits included) so the drivers compare like for like; the cached
// share is reported on its own.
func applyKimiUsage(stats *report.RunStats, usage []kimiUsage, st *kimiStream) {
	stats.Turns = max(len(usage), st.turns)
	for i, u := range usage {
		in := u.InputOther + u.InputCacheRead + u.InputCacheCreation
		stats.InputTokens += in
		stats.CachedInputTokens += u.InputCacheRead
		stats.OutputTokens += u.Output
		stats.PeakInput = max(stats.PeakInput, in)
		ts := report.TurnStat{Turn: i + 1, InputTokens: in, OutputTokens: u.Output}
		stop := "end_turn"
		if i < len(st.turnCalls) {
			ts.ToolCalls = st.turnCalls[i]
			if st.turnTools[i] > 0 {
				stop = "tool_use"
			}
		}
		stats.PerTurn = append(stats.PerTurn, ts)
		log.Printf("turn %d: stop=%s in=%d out=%d pruned=0B", i+1, stop, in, u.Output)
	}
}

// auditLog adapts a replayed audit file to failRun.
type auditLog []mcpbridge.CallRecord

func (a auditLog) AuditLog() []mcpbridge.CallRecord { return a }
