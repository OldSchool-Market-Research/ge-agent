// Package mcpbridge spawns ge-mcp as a child process (DESIGN.md's stdio
// contract: the agent owns the process) and bridges its tools to the loop.
// Every call is recorded: the audit log is what makes the directive's "cite
// only real tool output" guardrail auditable rather than aspirational.
package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ToolDef is an Anthropic-format tool definition. InputSchema is the MCP
// input schema passed through verbatim — both sides speak JSON Schema.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// CallRecord is one audited tool call.
type CallRecord struct {
	Seq      int             `json:"seq"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
	Result   string          `json:"result"`
	IsError  bool            `json:"is_error"`
	Duration time.Duration   `json:"duration_ms"`
	At       time.Time       `json:"at"`
}

type Bridge struct {
	c *client.Client

	mu  sync.Mutex
	log []CallRecord
}

// New spawns the ge-mcp binary and completes the MCP handshake. The child
// gets only the env it needs — the DSN — not the parent's environment.
func New(ctx context.Context, geMcpPath, dsn string) (*Bridge, error) {
	c, err := client.NewStdioMCPClient(geMcpPath, []string{"GE_MCP_DSN=" + dsn})
	if err != nil {
		return nil, fmt.Errorf("spawn ge-mcp: %w", err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "ge-agent", Version: "0.1.0"}
	if _, err := c.Initialize(ctx, req); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize ge-mcp: %w", err)
	}
	return &Bridge{c: c}, nil
}

// Tools lists ge-mcp's tools converted to Anthropic tool definitions.
func (b *Bridge) Tools(ctx context.Context) ([]ToolDef, error) {
	res, err := b.c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	defs := make([]ToolDef, 0, len(res.Tools))
	for _, t := range res.Tools {
		schema, err := t.InputSchema.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("tool %s schema: %w", t.Name, err)
		}
		defs = append(defs, ToolDef{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return defs, nil
}

// Call invokes a ge-mcp tool, returns the concatenated text content and the
// tool-level error flag, and records the call in the audit log.
func (b *Bridge) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	start := time.Now()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	var argMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return "", false, fmt.Errorf("tool %s: bad args json: %w", name, err)
		}
	}
	req.Params.Arguments = argMap

	res, err := b.c.CallTool(ctx, req)
	if err != nil {
		return "", false, fmt.Errorf("call %s: %w", name, err)
	}
	var text string
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			text += tc.Text
		}
	}
	b.record(CallRecord{
		Tool: name, Args: args, Result: text, IsError: res.IsError,
		Duration: time.Since(start).Round(time.Millisecond), At: start.UTC(),
	})
	return text, res.IsError, nil
}

func (b *Bridge) record(r CallRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r.Seq = len(b.log) + 1
	b.log = append(b.log, r)
}

// AuditLog returns a copy of every call made this run, in order.
func (b *Bridge) AuditLog() []CallRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]CallRecord, len(b.log))
	copy(out, b.log)
	return out
}

func (b *Bridge) Close() error { return b.c.Close() }
