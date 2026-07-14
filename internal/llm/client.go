// Package llm is a minimal Anthropic-messages-format client pointed at
// MiniMax's compatible endpoint. Hand-rolled (the plan's anticipated fallback,
// taken deliberately): the loop only needs messages + tools, no streaming, and
// raw JSON lets ge-mcp tool schemas and MiniMax's content blocks (including
// reasoning blocks) pass through verbatim instead of fighting SDK types.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string // e.g. https://api.minimax.io/anthropic
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// Message content is kept as raw JSON: assistant turns are echoed back into
// history byte-for-byte (preserving reasoning blocks we don't interpret), and
// user turns are built with MakeContent.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Block is the subset of a content block the loop interprets.
type Block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type Response struct {
	Content    json.RawMessage `json:"content"` // raw, echoed into history
	StopReason string          `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`

	Blocks []Block `json:"-"` // parsed view of Content
}

type request struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    string            `json:"system,omitempty"`
	Messages  []Message         `json:"messages"`
	Tools     []json.RawMessage `json:"tools,omitempty"`
}

type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ToolResult builds one tool_result block.
func ToolResult(toolUseID, content string, isError bool) map[string]any {
	b := map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": content}
	if isError {
		b["is_error"] = true
	}
	return b
}

// MakeContent marshals user-side content blocks into a Message body.
func MakeContent(blocks ...any) json.RawMessage {
	raw, err := json.Marshal(blocks)
	if err != nil {
		panic(err) // blocks are loop-constructed maps; cannot fail
	}
	return raw
}

func TextContent(text string) json.RawMessage {
	return MakeContent(map[string]any{"type": "text", "text": text})
}

// Send posts one messages request, retrying transient failures (429/5xx,
// network errors) with linear backoff.
func (c *Client) Send(ctx context.Context, system string, msgs []Message, tools []json.RawMessage, maxTokens int) (*Response, error) {
	body, err := json.Marshal(request{
		Model: c.Model, MaxTokens: maxTokens, System: system, Messages: msgs, Tools: tools,
	})
	if err != nil {
		return nil, err
	}

	const attempts = 4
	var lastErr error
	for i := 1; i <= attempts; i++ {
		resp, retryable, err := c.post(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable || i == attempts {
			break
		}
		wait := time.Duration(i) * 5 * time.Second
		log.Printf("llm: attempt %d/%d failed (%v), retrying in %s", i, attempts, err, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (c *Client) post(ctx context.Context, body []byte) (*Response, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Authorization", "Bearer "+c.APIKey) // MiniMax accepts either; send both
	req.Header.Set("anthropic-version", "2023-06-01")

	httpResp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, true, err
	}
	if httpResp.StatusCode != http.StatusOK {
		var ae apiError
		msg := string(respBody)
		if json.Unmarshal(respBody, &ae) == nil && ae.Error.Message != "" {
			msg = ae.Error.Type + ": " + ae.Error.Message
		}
		retryable := httpResp.StatusCode == 429 || httpResp.StatusCode >= 500
		return nil, retryable, fmt.Errorf("llm http %d: %s", httpResp.StatusCode, truncate(msg, 500))
	}

	var out Response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, false, fmt.Errorf("llm: bad response json: %w", err)
	}
	if err := json.Unmarshal(out.Content, &out.Blocks); err != nil {
		return nil, false, fmt.Errorf("llm: bad content blocks: %w", err)
	}
	return &out, false, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
