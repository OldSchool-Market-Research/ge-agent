package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
	"github.com/osrs-ge/ge-agent/internal/strategy"
)

// fakeBackend stands in for the ge-mcp bridge: every call succeeds with a
// small envelope except the tool named "boom", a transport failure, and
// "slow", which only returns once its call is cancelled.
type fakeBackend struct {
	mu  sync.Mutex
	log []mcpbridge.CallRecord
}

func (f *fakeBackend) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	if name == "slow" {
		<-ctx.Done() // a call kimi gives up on: the bridge sees its context end
		return "", false, fmt.Errorf("call slow: transport error: %w", ctx.Err())
	}
	if name == "boom" {
		return "", false, errors.New("call boom: broken pipe")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	text := `{"rows":[]}`
	f.log = append(f.log, mcpbridge.CallRecord{Seq: len(f.log) + 1, Tool: name, Args: args, Result: text, At: time.Now().UTC()})
	return text, false, nil
}

func (f *fakeBackend) AuditLog() []mcpbridge.CallRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mcpbridge.CallRecord(nil), f.log...)
}

const validReport = "## Header\nh\n## Digest\nd\n## Strategies\nnone\n## Proof\np\n## Discarded\nx\n"

func testHandoff(t *testing.T) proxyHandoff {
	dir := t.TempDir()
	return proxyHandoff{Dir: dir, ReportPath: filepath.Join(dir, "reports", "20261030-0100.md"), RunStart: time.Now().UTC()}
}

func mustProxy(t *testing.T, h proxyHandoff) *proxy {
	t.Helper()
	p, err := newProxy(&fakeBackend{}, h)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func callReq(name, args string) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Name = name
	req.Params.RawArguments = json.RawMessage(args)
	return req
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want one content block, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("want text content, got %T", res.Content[0])
	}
	return tc.Text
}

func readResult(t *testing.T, h proxyHandoff) *proxyResult {
	t.Helper()
	var res *proxyResult
	if err := readJSON(h.resultPath(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestProxySubmitAcceptsAndWritesArtifacts(t *testing.T) {
	h := testHandoff(t)
	p := mustProxy(t, h)
	ctx := context.Background()
	if _, err := p.callTool(ctx, callReq("quote", `{"name_or_id":"Abyssal whip"}`)); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"markdown": validReport, "strategies": []any{}})
	res, err := p.submit(ctx, callReq("submit_report", string(args)))
	if err != nil || res.IsError {
		t.Fatalf("valid report rejected: %v / %s", err, resultText(t, res))
	}
	if got := readResult(t, h); got == nil || got.Outcome != "submitted" || got.ReportPath != h.ReportPath {
		t.Fatalf("outcome %+v, want submitted at %s", got, h.ReportPath)
	}
	md, err := os.ReadFile(h.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "Abyssal whip") {
		t.Error("report appendix is missing the witnessed tool call")
	}
	if _, err := os.Stat(strings.TrimSuffix(h.ReportPath, ".md") + ".strategies.json"); err != nil {
		t.Errorf("sidecar: %v", err)
	}
	// The run is settled: later calls are refused, not executed.
	res, _ = p.callTool(ctx, callReq("quote", `{}`))
	if !res.IsError || !strings.Contains(resultText(t, res), "run is over") {
		t.Errorf("call after acceptance: %q", resultText(t, res))
	}
}

func TestProxyFailsOnThirdSameRejection(t *testing.T) {
	h := testHandoff(t)
	p := mustProxy(t, h)
	bad := callReq("submit_report", `{"markdown":"## Header only","strategies":[]}`)
	for i := 1; i <= 2; i++ {
		res, _ := p.submit(context.Background(), bad)
		text := resultText(t, res)
		if !res.IsError || !strings.Contains(text, `"code":"invalid_report"`) {
			t.Fatalf("attempt %d: want an invalid_report rejection, got %q", i, text)
		}
		if i == 2 && !strings.Contains(text, "SAME REJECTION") {
			t.Errorf("attempt 2 should escalate the hint: %q", text)
		}
	}
	if readResult(t, h) != nil {
		t.Fatal("two rejections must not settle the run")
	}
	res, _ := p.submit(context.Background(), bad)
	if !strings.HasPrefix(resultText(t, res), "FATAL: ") {
		t.Errorf("third rejection: %q", resultText(t, res))
	}
	got := readResult(t, h)
	// The orchestrator classifies failures by this wording (api/failkind.go).
	if got == nil || got.Outcome != "failed" || !strings.HasPrefix(got.FailReason, "report rejected 3 times in a row for the same reason: ") {
		t.Fatalf("outcome %+v", got)
	}
}

func TestProxyTransportErrorFailsRun(t *testing.T) {
	h := testHandoff(t)
	p := mustProxy(t, h)
	res, _ := p.callTool(context.Background(), callReq("boom", `{}`))
	if !res.IsError || !strings.HasPrefix(resultText(t, res), "FATAL: tool boom: ") {
		t.Errorf("result %q", resultText(t, res))
	}
	if got := readResult(t, h); got == nil || got.Outcome != "failed" || !strings.HasPrefix(got.FailReason, "tool boom: ") {
		t.Fatalf("outcome %+v", got)
	}
}

// A nudge resumes the kimi session with a fresh proxy: the audit log and the
// rejection streak must carry over, numbered and counted across both.
func TestProxyStateSurvivesRestart(t *testing.T) {
	h := testHandoff(t)
	ctx := context.Background()
	bad := callReq("submit_report", `{"markdown":"## Header only","strategies":[]}`)

	p1 := mustProxy(t, h)
	p1.callTool(ctx, callReq("quote", `{"n":1}`))
	p1.callTool(ctx, callReq("quotes", `{"n":2}`))
	p1.submit(ctx, bad)

	p2 := mustProxy(t, h)
	p2.callTool(ctx, callReq("liquidity", `{"n":3}`))
	log := p2.AuditLog()
	if len(log) != 3 || log[0].Tool != "quote" || log[2].Tool != "liquidity" {
		t.Fatalf("audit across proxies: %+v", log)
	}
	for i, r := range log {
		if r.Seq != i+1 {
			t.Errorf("record %d has seq %d", i, r.Seq)
		}
	}
	onDisk, err := readAudit(h.auditPath())
	if err != nil || len(onDisk) != 3 || onDisk[2].Seq != 3 {
		t.Fatalf("audit file: %v / %+v", err, onDisk)
	}
	res, _ := p2.submit(ctx, bad)
	if !strings.Contains(resultText(t, res), "SAME REJECTION") {
		t.Errorf("the streak did not carry over: %q", resultText(t, res))
	}
}

func TestProxyServesSubmitReport(t *testing.T) {
	p := mustProxy(t, testHandoff(t))
	srv := p.server([]mcpbridge.ToolDef{{Name: "quote", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	tools := srv.ListTools()
	if tools["quote"] == nil || tools["submit_report"] == nil || len(tools) != 2 {
		t.Fatalf("tools: %v", tools)
	}
	if !strings.Contains(string(tools["submit_report"].Tool.RawInputSchema), `"strategies"`) {
		t.Error("submit_report must carry the report schema")
	}
}

func TestProxyCancelledCallKeepsRunAlive(t *testing.T) {
	h := testHandoff(t)
	p := mustProxy(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _ := p.callTool(ctx, callReq("slow", `{}`))
	if !res.IsError || strings.HasPrefix(resultText(t, res), fatalPrefix) {
		t.Errorf("result %q", resultText(t, res))
	}
	if got := readResult(t, h); got != nil {
		t.Fatalf("a cancelled call settled the run: %+v", got)
	}
	if res, _ := p.callTool(context.Background(), callReq("quotes", `{}`)); res.IsError {
		t.Errorf("the run is over after a cancelled call: %q", resultText(t, res))
	}
}

// countingBackend counts ge-mcp calls per tool; "bad" answers with a tool error.
type countingBackend struct {
	fakeBackend
	calls map[string]int
}

func (c *countingBackend) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	c.mu.Lock()
	c.calls[name]++
	c.mu.Unlock()
	if name == "bad" {
		return `{"error":"item_not_found"}`, true, nil
	}
	return c.fakeBackend.Call(ctx, name, args)
}

func TestProxyCachesIdenticalCallsForAMinute(t *testing.T) {
	b := &countingBackend{calls: map[string]int{}}
	p, err := newProxy(b, testHandoff(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	ctx := context.Background()

	p.callTool(ctx, callReq("combo_quote", `{"relation_id":7,"direction":"forward"}`))
	now = now.Add(59 * time.Second)
	// Same arguments in another key order: answered from the cache.
	res, _ := p.callTool(ctx, callReq("combo_quote", `{"direction":"forward","relation_id":7}`))
	if res.IsError || resultText(t, res) != `{"rows":[]}` {
		t.Fatalf("cached result %q", resultText(t, res))
	}
	if b.calls["combo_quote"] != 1 {
		t.Fatalf("a repeat inside the TTL reached ge-mcp: %d calls", b.calls["combo_quote"])
	}
	p.callTool(ctx, callReq("combo_quote", `{"relation_id":7,"direction":"reverse"}`))
	if b.calls["combo_quote"] != 2 {
		t.Fatalf("different arguments must miss: %d calls", b.calls["combo_quote"])
	}
	now = now.Add(2 * time.Second) // 61s after the first call
	p.callTool(ctx, callReq("combo_quote", `{"relation_id":7,"direction":"forward"}`))
	if b.calls["combo_quote"] != 3 {
		t.Fatalf("an expired entry must refetch: %d calls", b.calls["combo_quote"])
	}

	p.callTool(ctx, callReq("bad", `{}`))
	p.callTool(ctx, callReq("bad", `{}`))
	if b.calls["bad"] != 2 {
		t.Fatalf("tool errors must not be cached: %d calls", b.calls["bad"])
	}
}

func TestPrecomputedScan(t *testing.T) {
	cases := []struct {
		tool, args string
		refused    bool
	}{
		{"volume_zscore", `{"window":"1h","limit":25}`, true}, // default baseline is same_how
		{"volume_zscore", `{"baseline":"same_how"}`, true},
		{"volume_zscore", `{"baseline":"trailing","limit":25}`, false},
		{"volume_zscore", `{"name_or_id":"Fish pie","baseline":"same_how"}`, false},
		{"seasonal_scan", `{"limit":15}`, true},
		{"seasonality", `{"name_or_id":"Fish pie"}`, false},
		{"top_flips", `{"min_vol24h":100000}`, false},
	}
	for _, c := range cases {
		if got := precomputedScan(c.tool, json.RawMessage(c.args)) != ""; got != c.refused {
			t.Errorf("%s %s: refused=%v, want %v", c.tool, c.args, got, c.refused)
		}
	}
}

func TestProxyRefusesPrecomputedScansOnlyWhenTold(t *testing.T) {
	ctx := context.Background()
	b := &countingBackend{calls: map[string]int{}}
	p, err := newProxy(b, testHandoff(t))
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := p.callTool(ctx, callReq("seasonal_scan", `{}`)); res.IsError {
		t.Fatalf("without the flag the scan runs: %q", resultText(t, res))
	}
	p.precomputed = true
	res, _ := p.callTool(ctx, callReq("volume_zscore", `{"limit":25}`))
	if !res.IsError || !strings.Contains(resultText(t, res), "Latest market sweep") {
		t.Fatalf("result %q", resultText(t, res))
	}
	if b.calls["volume_zscore"] != 0 {
		t.Fatal("a refused scan reached ge-mcp")
	}
}

// slowClockBackend advances the proxy's clock while a call runs.
type slowClockBackend struct {
	fakeBackend
	advance func()
}

func (s *slowClockBackend) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	s.advance()
	return s.fakeBackend.Call(ctx, name, args)
}

// A slow call's data is as old as the call's start: its TTL counts from there.
func TestProxyCacheAgesFromCallStart(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	b := &slowClockBackend{advance: func() { now = now.Add(50 * time.Second) }}
	p, err := newProxy(b, testHandoff(t))
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }
	ctx := context.Background()
	p.callTool(ctx, callReq("top_flips", `{}`)) // starts at 0s, answers at 50s
	now = now.Add(11 * time.Second)             // 61s after it started
	p.callTool(ctx, callReq("top_flips", `{}`))
	if got := len(b.AuditLog()); got != 2 {
		t.Fatalf("an entry 61s past its start must refetch: %d backend calls", got)
	}
}

// The Kimi CLI validates a tool call's arguments against this schema before the
// call reaches the proxy, and rejects the whole call on a miss. A proposal is
// optional, so its shape must stay in the description: a required field or an enum
// here would let one bad proposal cost the report, past the reach of the 3-strike.
func TestProposalsSchemaStaysLoose(t *testing.T) {
	var def struct {
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"input_schema"`
	}
	if err := json.Unmarshal(submitReportDef, &def); err != nil {
		t.Fatal(err)
	}
	raw, ok := def.InputSchema.Properties["proposals"]
	if !ok {
		t.Fatal("submit_report has no proposals field")
	}
	var field struct {
		Items map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &field); err != nil {
		t.Fatal(err)
	}
	if len(field.Items) != 1 || field.Items["type"] != "object" {
		t.Errorf("proposals items must be a bare object, got %v", field.Items)
	}
}

// A malformed proposal costs neither the report nor the record that it happened.
func TestProxyCountsDroppedProposals(t *testing.T) {
	h := testHandoff(t)
	p := mustProxy(t, h)
	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{"markdown": validReport, "strategies": []any{},
		"proposals": []any{map[string]any{"kind": "lane", "title": "t", "detail": "d"}, map[string]any{"kind": "nonsense", "title": "t", "detail": "d"}}})
	res, err := p.submit(ctx, callReq("submit_report", string(args)))
	if err != nil || res.IsError {
		t.Fatalf("report rejected over a proposal: %v / %s", err, resultText(t, res))
	}
	raw, err := os.ReadFile(strings.TrimSuffix(h.ReportPath, ".md") + ".strategies.json")
	if err != nil {
		t.Fatal(err)
	}
	var sc strategy.Sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.Proposals) != 1 || sc.ProposalsDropped != 1 {
		t.Errorf("proposals %d dropped %d, want 1 and 1", len(sc.Proposals), sc.ProposalsDropped)
	}
}
