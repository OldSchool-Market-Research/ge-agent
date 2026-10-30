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
	"path/filepath"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-agent/internal/config"
	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
)

// The kimi driver's MCP half. The Kimi CLI spawns `ge-agent -mcp-proxy` as
// its only MCP server; the proxy fronts ge-mcp through mcpbridge (whose
// audit log becomes the report appendix) and serves submit_report behind the
// report gate: validation, rejection payloads and the 3-strike fail-fast.
// Its stdout is the MCP channel — all logging goes to stderr.
//
// A nudge resumes the kimi session, which spawns a fresh proxy, so whatever
// must outlive one proxy lives in the handoff dir: the audit log (appended
// per call), the rejection streak, and the run's outcome.

const (
	envProxyDir    = "GE_AGENT_PROXY_DIR"
	envProxyReport = "GE_AGENT_PROXY_REPORT"
	envProxyStart  = "GE_AGENT_PROXY_START"

	runOverMsg = "The run is over (the report was accepted, or the run failed). Stop now: do not call any more tools."
	// Settlement messages; the driver stops kimi on seeing them.
	acceptedMsg = "Report accepted. The run is complete: stop now and do not call any more tools."
	fatalPrefix = "FATAL: "
	fatalSuffix = " -- the run has failed; stop now."
)

// proxyHandoff is what Run tells the proxy it spawns (through kimi's
// environment, which MCP servers inherit).
type proxyHandoff struct {
	Dir        string
	ReportPath string // absolute
	RunStart   time.Time
}

func (h proxyHandoff) env() []string {
	return []string{
		envProxyDir + "=" + h.Dir,
		envProxyReport + "=" + h.ReportPath,
		envProxyStart + "=" + h.RunStart.Format(time.RFC3339Nano),
	}
}

func handoffFromEnv() (proxyHandoff, error) {
	h := proxyHandoff{Dir: os.Getenv(envProxyDir), ReportPath: os.Getenv(envProxyReport)}
	start, err := time.Parse(time.RFC3339Nano, os.Getenv(envProxyStart))
	if h.Dir == "" || h.ReportPath == "" || err != nil {
		return h, fmt.Errorf("-mcp-proxy is spawned by the kimi driver: %s, %s and %s must be set", envProxyDir, envProxyReport, envProxyStart)
	}
	h.RunStart = start
	return h, nil
}

func (h proxyHandoff) resultPath() string { return filepath.Join(h.Dir, "result.json") }
func (h proxyHandoff) auditPath() string  { return filepath.Join(h.Dir, "audit.jsonl") }
func (h proxyHandoff) streakPath() string { return filepath.Join(h.Dir, "reject-streak.json") }

// proxyResult is the run's outcome as the proxy saw it. No file = the model
// never finished: the driver decides why (turn cap, timeout, no submit).
type proxyResult struct {
	Outcome    string `json:"outcome"` // submitted | failed
	ReportPath string `json:"report_path,omitempty"`
	FailReason string `json:"fail_reason,omitempty"`
}

type rejectStreak struct {
	Last string `json:"last"`
	Same int    `json:"same"`
}

// toolBackend is the bridge to ge-mcp (a fake in tests).
type toolBackend interface {
	Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error)
	AuditLog() []mcpbridge.CallRecord
}

type proxy struct {
	backend toolBackend
	h       proxyHandoff
	prior   []mcpbridge.CallRecord // calls made by earlier proxies this run

	mu      sync.Mutex
	written int // backend records already appended to the audit file
	streak  rejectStreak
	over    bool
}

// ServeProxy runs the MCP proxy until kimi closes its stdin.
func ServeProxy(ctx context.Context, cfg *config.Config) error {
	h, err := handoffFromEnv()
	if err != nil {
		return err
	}
	bridge, err := mcpbridge.New(ctx, cfg.GeMcpPath, cfg.GeMcpDSN)
	if err != nil {
		return err
	}
	defer bridge.Close()
	tools, err := bridge.Tools(ctx)
	if err != nil {
		return err
	}
	p, err := newProxy(bridge, h)
	if err != nil {
		return err
	}
	return server.NewStdioServer(p.server(tools)).Listen(ctx, os.Stdin, os.Stdout)
}

func newProxy(backend toolBackend, h proxyHandoff) (*proxy, error) {
	prior, err := readAudit(h.auditPath())
	if err != nil {
		return nil, err
	}
	p := &proxy{backend: backend, h: h, prior: prior}
	if err := readJSON(h.streakPath(), &p.streak); err != nil {
		return nil, err
	}
	if _, err := os.Stat(h.resultPath()); err == nil {
		p.over = true
	}
	return p, nil
}

// server registers ge-mcp's tools (schemas passed through verbatim) plus
// submit_report.
func (p *proxy) server(tools []mcpbridge.ToolDef) *server.MCPServer {
	srv := server.NewMCPServer("ge-agent", "kimi-proxy", server.WithToolCapabilities(false))
	for _, t := range tools {
		srv.AddTool(mcp.NewToolWithRawSchema(t.Name, t.Description, t.InputSchema), p.callTool)
	}
	var def mcpbridge.ToolDef
	if err := json.Unmarshal(submitReportDef, &def); err != nil {
		panic(err) // a compile-time constant
	}
	srv.AddTool(mcp.NewToolWithRawSchema(def.Name, def.Description, def.InputSchema), p.submit)
	return srv
}

func (p *proxy) callTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if p.isOver() {
		return mcp.NewToolResultError(runOverMsg), nil
	}
	args, err := rawArgs(req)
	if err != nil {
		return mcp.NewToolResultError("arguments are not JSON: " + err.Error()), nil
	}
	text, isErr, err := p.backend.Call(ctx, req.Params.Name, args)
	if err != nil {
		// A transport-level ge-mcp failure is not something the model can fix,
		// so the run fails.
		reason := fmt.Sprintf("tool %s: %v", req.Params.Name, err)
		p.finish(proxyResult{Outcome: "failed", FailReason: reason})
		return mcp.NewToolResultError(fatalPrefix + reason + fatalSuffix), nil
	}
	p.mu.Lock()
	p.flushAuditLocked()
	p.mu.Unlock()
	log.Printf("  tool %s (err=%v, %d bytes)", req.Params.Name, isErr, len(text))
	res := mcp.NewToolResultText(text)
	res.IsError = isErr
	return res, nil
}

func (p *proxy) submit(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.over {
		return mcp.NewToolResultError(runOverMsg), nil
	}
	input, err := rawArgs(req)
	if err != nil {
		return mcp.NewToolResultError("arguments are not JSON: " + err.Error()), nil
	}
	path, gateErr := handleSubmit(input, p.h.ReportPath, p.h.RunStart, p)
	if gateErr == "" {
		log.Printf("report accepted: %s", path)
		p.finishLocked(proxyResult{Outcome: "submitted", ReportPath: path})
		return mcp.NewToolResultText(acceptedMsg), nil
	}
	log.Printf("report rejected: %s", gateErr)
	if gateErr == p.streak.Last {
		p.streak.Same++
	} else {
		p.streak = rejectStreak{Last: gateErr, Same: 1}
	}
	if err := writeJSONAtomic(p.h.streakPath(), p.streak); err != nil {
		log.Printf("could not persist the rejection streak: %v", err)
	}
	if p.streak.Same >= maxSameRejections {
		reason := fmt.Sprintf("report rejected %d times in a row for the same reason: %s", p.streak.Same, gateErr)
		p.finishLocked(proxyResult{Outcome: "failed", FailReason: reason})
		return mcp.NewToolResultError(fatalPrefix + reason + fatalSuffix), nil
	}
	res := mcp.NewToolResultText(rejectionPayload(gateErr, p.streak.Same))
	res.IsError = true
	return res, nil
}

// rawArgs is the call's arguments exactly as the model wrote them (as the api
// loop passes tool_use input): strategy.Parse repairs quoted numbers on the
// raw bytes, and the audit log records what was actually sent.
func rawArgs(req mcp.CallToolRequest) (json.RawMessage, error) {
	if len(req.Params.RawArguments) > 0 {
		return req.Params.RawArguments, nil
	}
	return json.Marshal(req.Params.Arguments)
}

// AuditLog is every call made this run — earlier proxies' first — numbered
// 1..n across them.
func (p *proxy) AuditLog() []mcpbridge.CallRecord {
	cur := p.backend.AuditLog()
	out := make([]mcpbridge.CallRecord, 0, len(p.prior)+len(cur))
	out = append(out, p.prior...)
	for _, r := range cur {
		r.Seq = len(out) + 1
		out = append(out, r)
	}
	return out
}

func (p *proxy) isOver() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.over
}

func (p *proxy) finish(res proxyResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishLocked(res)
}

// finishLocked ends the run: later calls are refused, and the outcome is on
// disk for the driver. The first outcome wins.
func (p *proxy) finishLocked(res proxyResult) {
	if p.over {
		return
	}
	p.over = true
	p.flushAuditLocked()
	if err := writeJSONAtomic(p.h.resultPath(), res); err != nil {
		log.Printf("could not write the run outcome: %v", err)
	}
}

// flushAuditLocked appends the backend's not-yet-written calls to the audit
// file, numbered after the prior proxies' calls.
func (p *proxy) flushAuditLocked() {
	cur := p.backend.AuditLog()
	if p.written >= len(cur) {
		return
	}
	f, err := os.OpenFile(p.h.auditPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("could not append the audit log: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range cur[p.written:] {
		r.Seq = len(p.prior) + p.written + 1
		if err := enc.Encode(r); err != nil {
			log.Printf("could not append the audit log: %v", err)
			return
		}
		p.written++
	}
}

func readAudit(path string) ([]mcpbridge.CallRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []mcpbridge.CallRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var r mcpbridge.CallRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("audit log %s: %w", path, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// readJSON leaves v untouched when the file does not exist.
func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func writeJSONAtomic(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
