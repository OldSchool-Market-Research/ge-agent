package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osrs-ge/ge-agent/internal/config"
	"github.com/osrs-ge/ge-agent/internal/report"
)

func assistant(calls ...string) []byte {
	var tcs []map[string]any
	for i, name := range calls {
		tcs = append(tcs, map[string]any{"id": name + "-" + string(rune('a'+i)), "function": map[string]string{"name": name}})
	}
	raw, _ := json.Marshal(map[string]any{"role": "assistant", "tool_calls": tcs})
	return raw
}

func toolResult(id, content string) []byte {
	raw, _ := json.Marshal(map[string]string{"role": "tool", "tool_call_id": id, "content": content})
	return raw
}

func TestKimiStreamTurnCap(t *testing.T) {
	st := newKimiStream(2)
	if st.feed(assistant("mcp__ge__quote", "mcp__ge__quotes")) || st.feed(assistant("mcp__ge__liquidity")) {
		t.Fatal("stopped inside the budget")
	}
	if !st.feed(assistant("mcp__ge__quote")) || !st.capHit {
		t.Fatal("a third turn on a budget of 2 must stop kimi")
	}
	if st.turns != 2 || len(st.turnCalls) != 2 || st.turnCalls[0] != 2 {
		t.Errorf("turns %d, calls %v", st.turns, st.turnCalls)
	}
}

func TestKimiStreamRejectionGrantsGraceOnce(t *testing.T) {
	st := newKimiStream(10)
	st.feed(assistant("mcp__ge__submit_report"))
	rej := rejectionPayload("strategies[0].id: must match", 1)
	st.feed(toolResult("mcp__ge__submit_report-a", rej))
	if st.maxTurns != 10+graceTurns {
		t.Fatalf("budget %d after a rejection", st.maxTurns)
	}
	st.feed(assistant("mcp__ge__submit_report"))
	st.feed(toolResult("mcp__ge__submit_report-a", "Error: "+rej)) // wrapped by kimi
	if st.maxTurns != 10+graceTurns {
		t.Errorf("grace granted twice: budget %d", st.maxTurns)
	}
	if st.turnCalls[0] != 0 || st.turnTools[0] != 1 {
		t.Errorf("submit_report is not a ge-mcp call: %v / %v", st.turnCalls, st.turnTools)
	}
}

func TestKimiStreamStopsOnceSettled(t *testing.T) {
	st := newKimiStream(80)
	st.feed(assistant("mcp__ge__submit_report"))
	if !st.feed(toolResult("mcp__ge__submit_report-a", "Report accepted. The run is complete")) || !st.finished {
		t.Fatal("an accepted report must stop kimi")
	}
	st = newKimiStream(80)
	st.feed(assistant("mcp__ge__quote"))
	if !st.feed(toolResult("mcp__ge__quote-a", "FATAL: tool quote: broken pipe -- the run has failed")) {
		t.Fatal("a fatal proxy error must stop kimi")
	}
}

func TestKimiStreamSessionID(t *testing.T) {
	st := newKimiStream(80)
	st.feed([]byte(`{"role":"meta","type":"session.resume_hint","session_id":"session_abc","command":"kimi -r session_abc"}`))
	if st.sessionID != "session_abc" {
		t.Errorf("session %q", st.sessionID)
	}
}

func TestReadKimiUsage(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "wd_kimi_1234", "session_abc", "agents", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	wire := strings.Join([]string{
		`{"type":"llm.request","model":"k3.1-pro"}`,
		`{"type":"usage.record","usage":{"inputOther":23163,"output":286,"inputCacheRead":0,"inputCacheCreation":0}}`,
		`{"type":"token_counting.measured","usage":{"inputOther":1}}`,
		`{"type":"usage.record","usage":{"inputOther":536,"output":91,"inputCacheRead":23040,"inputCacheCreation":0}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "wire.jsonl"), []byte(wire), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := readKimiUsage(home, "session_abc")
	if err != nil || len(usage) != 2 {
		t.Fatalf("usage %+v, err %v", usage, err)
	}

	st := newKimiStream(80)
	st.feed(assistant("mcp__ge__quote"))
	st.feed(assistant())
	var stats report.RunStats
	applyKimiUsage(&stats, usage, st)
	if stats.Turns != 2 || stats.InputTokens != 23163+536+23040 || stats.CachedInputTokens != 23040 ||
		stats.OutputTokens != 377 || stats.PeakInput != 23576 {
		t.Errorf("stats %+v", stats)
	}
	if stats.PerTurn[0].ToolCalls != 1 || stats.PerTurn[1].ToolCalls != 0 {
		t.Errorf("per turn %+v", stats.PerTurn)
	}
}

// Stopping kimi on acceptance skips its resume hint, so the session is found
// by workspace instead.
func TestFindKimiSession(t *testing.T) {
	home := t.TempDir()
	for _, s := range []struct {
		ws, id, cwd string
		at          int64
	}{
		{"wd_kimi_1", "session_old", "/runs/1/kimi", 100},
		{"wd_kimi_1", "session_new", "/runs/1/kimi", 200},
		{"wd_kimi_2", "session_other", "/runs/2/kimi", 300},
	} {
		dir := filepath.Join(home, "sessions", s.ws, s.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"id": s.id, "cwd": s.cwd, "createdAt": s.at})
		if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := findKimiSession(home, "/runs/1/kimi"); got != "session_new" {
		t.Errorf("got %q", got)
	}
	if got := findKimiSession(home, "/runs/9/kimi"); got != "" {
		t.Errorf("unknown workspace matched %q", got)
	}
}

func TestKimiWorkspaceHasNoSecretsAndOnlyGeTools(t *testing.T) {
	ws := t.TempDir()
	if err := writeKimiWorkspace(ws, "SYSTEM PROMPT", "/bin/ge-agent"); err != nil {
		t.Fatal(err)
	}
	agent, _ := os.ReadFile(filepath.Join(ws, kimiAgentFile))
	if !strings.Contains(string(agent), "\ntools: mcp__ge__*\n") || !strings.HasSuffix(string(agent), "SYSTEM PROMPT\n") {
		t.Errorf("agent file:\n%s", agent)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(filepath.Join(ws, ".kimi-code", "mcp.json"))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	ge := cfg.MCPServers["ge"]
	if len(cfg.MCPServers) != 1 || ge.Command != "/bin/ge-agent" || len(ge.Args) != 1 || ge.Args[0] != "-mcp-proxy" || ge.Env != nil {
		t.Errorf("mcp.json: %s", raw)
	}
}

func TestKimiEnv(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "secret")
	t.Setenv("GE_MCP_DSN", "postgresql://x")
	h := proxyHandoff{Dir: "/ws", ReportPath: "/ws/r.md"}
	env := strings.Join(kimiEnv(&config.Config{KimiHome: "/kimi"}, "/ws", h), "\n")
	for _, want := range []string{"GE_MCP_DSN=postgresql://x", "KIMI_CODE_HOME=/kimi", "KIMI_CODE_TRUST_WORKSPACE=1", "KIMI_CODE_NO_AUTO_UPDATE=1", "GE_AGENT_DRIVER=kimi", envProxyDir + "=/ws"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	if strings.Contains(env, "secret") {
		t.Error("the MiniMax key leaked into kimi's environment")
	}
}

// fakeKimi records its arguments and plays one turn per invocation; from
// invocation $FAKE_KIMI_SUBMIT_ON on, it settles the run as the proxy would.
const fakeKimi = `#!/bin/sh
n=$(cat "$GE_AGENT_PROXY_DIR/calls" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$GE_AGENT_PROXY_DIR/calls"
echo "$@" >> "$GE_AGENT_PROXY_DIR/args"
echo '{"role":"assistant","tool_calls":[{"id":"t1","function":{"name":"mcp__ge__quote"}}]}'
echo '{"role":"tool","tool_call_id":"t1","content":"{\"rows\":[]}"}'
if [ "$n" -ge "$FAKE_KIMI_SUBMIT_ON" ]; then
  printf '{"outcome":"submitted","report_path":"%s"}' "$GE_AGENT_PROXY_REPORT" > "$GE_AGENT_PROXY_DIR/result.json"
fi
echo '{"role":"meta","type":"session.resume_hint","session_id":"session_fake"}'
`

func runFakeKimi(t *testing.T, submitOn string, maxTurns int) (*proxyResult, error, *kimiStream, []string) {
	t.Helper()
	ws := t.TempDir()
	bin := filepath.Join(t.TempDir(), "kimi")
	if err := os.WriteFile(bin, []byte(fakeKimi), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_KIMI_SUBMIT_ON", submitOn)
	h := proxyHandoff{Dir: ws, ReportPath: filepath.Join(ws, "r.md")}
	st := newKimiStream(maxTurns)
	res, err := driveKimi(context.Background(), &config.Config{KimiCmd: bin}, ws, h, st)
	args, _ := os.ReadFile(filepath.Join(ws, "args"))
	return res, err, st, strings.Split(strings.TrimSpace(string(args)), "\n")
}

func TestDriveKimiNudgesByResuming(t *testing.T) {
	res, err, st, args := runFakeKimi(t, "2", 80)
	if err != nil || res == nil || res.Outcome != "submitted" {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if len(args) != 2 || !strings.Contains(args[0], "--agent-file") || strings.Contains(args[0], "--session") {
		t.Fatalf("kickoff args: %v", args)
	}
	if !strings.Contains(args[1], "--session session_fake") || strings.Contains(args[1], "--agent-file") ||
		!strings.Contains(args[1], nudgePrompt) {
		t.Errorf("nudge args: %s", args[1])
	}
	if st.turns != 2 {
		t.Errorf("turns %d across the kickoff and one nudge", st.turns)
	}
}

func TestDriveKimiGivesUpAfterNudges(t *testing.T) {
	_, err, _, args := runFakeKimi(t, "99", 80)
	if err == nil || err.Error() != "model ended without submit_report after 2 nudges" {
		t.Fatalf("err %v", err)
	}
	if len(args) != 1+maxNudges {
		t.Errorf("%d invocations", len(args))
	}
}

func TestDriveKimiTurnCap(t *testing.T) {
	_, err, st, _ := runFakeKimi(t, "99", 1)
	if err == nil || !strings.HasPrefix(err.Error(), "MAX_TURNS (1") || !st.capHit {
		t.Fatalf("err %v", err)
	}
}
