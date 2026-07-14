// Package config loads ge-agent's env-only configuration (container-ready:
// no flags required, no files outside the working dir).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIKey     string        // MINIMAX_API_KEY, or `minimax=` from ./.env
	BaseURL    string        // GE_AGENT_BASE_URL
	Model      string        // GE_AGENT_MODEL
	GeMcpPath  string        // GE_MCP_PATH — the ge-mcp binary the agent spawns
	GeMcpDSN   string        // GE_MCP_DSN — passed through to the child
	ReportsDir string        // GE_AGENT_REPORTS_DIR
	MaxTurns   int           // GE_AGENT_MAX_TURNS
	MaxTokens  int           // GE_AGENT_MAX_TOKENS per response
	Directive  string        // GE_AGENT_DIRECTIVE — path to DIRECTIVE.md
	Timeout    time.Duration // GE_AGENT_RUN_TIMEOUT — whole-run ceiling
}

func Load() (*Config, error) {
	c := &Config{
		APIKey:     os.Getenv("MINIMAX_API_KEY"),
		BaseURL:    getenv("GE_AGENT_BASE_URL", "https://api.minimax.io/anthropic"),
		Model:      getenv("GE_AGENT_MODEL", "MiniMax-M3"),
		GeMcpPath:  os.Getenv("GE_MCP_PATH"),
		GeMcpDSN:   os.Getenv("GE_MCP_DSN"),
		ReportsDir: getenv("GE_AGENT_REPORTS_DIR", "reports"),
		MaxTurns:   getenvInt("GE_AGENT_MAX_TURNS", 50),
		MaxTokens:  getenvInt("GE_AGENT_MAX_TOKENS", 16384),
		Directive:  getenv("GE_AGENT_DIRECTIVE", "DIRECTIVE.md"),
		Timeout:    time.Duration(getenvInt("GE_AGENT_RUN_TIMEOUT_S", 3600)) * time.Second,
	}
	if c.APIKey == "" {
		c.APIKey = keyFromDotEnv(".env", "minimax")
	}
	switch {
	case c.APIKey == "":
		return nil, fmt.Errorf("no API key: set MINIMAX_API_KEY or `minimax=` in ./.env")
	case c.GeMcpPath == "":
		return nil, fmt.Errorf("GE_MCP_PATH not set (path to the ge-mcp binary)")
	case c.GeMcpDSN == "":
		return nil, fmt.Errorf("GE_MCP_DSN not set (passed through to ge-mcp)")
	}
	return c, nil
}

// keyFromDotEnv reads KEY=VALUE lines; returns "" when absent.
func keyFromDotEnv(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
