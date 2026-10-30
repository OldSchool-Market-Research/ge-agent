package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
)

// fakeBackend stands in for the ge-mcp bridge: every call succeeds with a
// small envelope except the tool named "boom", a transport failure.
type fakeBackend struct {
	mu  sync.Mutex
	log []mcpbridge.CallRecord
}

func (f *fakeBackend) Call(_ context.Context, name string, args json.RawMessage) (string, bool, error) {
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
