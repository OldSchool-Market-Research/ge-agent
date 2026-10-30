// Package config loads ge-agent's env-only configuration (container-ready:
// no flags required, no files outside the working dir).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	GeMcpPath  string        // GE_MCP_PATH — the ge-mcp binary the agent spawns
	GeMcpDSN   string        // GE_MCP_DSN — passed through to the child
	ReportsDir string        // GE_AGENT_REPORTS_DIR
	MaxTurns   int           // GE_AGENT_MAX_TURNS
	Directive  string        // GE_AGENT_DIRECTIVE — path to DIRECTIVE.md
	BriefFile  string        // GE_AGENT_BRIEF_FILE — optional per-run brief appended to the system prompt
	Timeout    time.Duration // GE_AGENT_RUN_TIMEOUT — whole-run ceiling

	// The model loop is the Kimi Code CLI, which authenticates with a
	// coding-plan login rather than an API key (the plan has no API access),
	// while ge-agent serves the tools to it as an MCP proxy (loop/kimi.go).
	KimiCmd   string // GE_AGENT_KIMI_CMD — the kimi CLI
	KimiHome  string // GE_AGENT_KIMI_HOME — KIMI_CODE_HOME (login, config, sessions); "" = the CLI's default
	KimiModel string // GE_AGENT_KIMI_MODEL — model alias, e.g. kimi-code/k3.1-pro; "" = the CLI's default_model
}

func Load() (*Config, error) {
	c := &Config{
		GeMcpPath:  os.Getenv("GE_MCP_PATH"),
		GeMcpDSN:   os.Getenv("GE_MCP_DSN"),
		ReportsDir: getenv("GE_AGENT_REPORTS_DIR", "reports"),
		MaxTurns:   getenvInt("GE_AGENT_MAX_TURNS", 50),
		Directive:  getenv("GE_AGENT_DIRECTIVE", "DIRECTIVE.md"),
		BriefFile:  os.Getenv("GE_AGENT_BRIEF_FILE"),
		Timeout:    time.Duration(getenvInt("GE_AGENT_RUN_TIMEOUT_S", 3600)) * time.Second,

		KimiCmd:   getenv("GE_AGENT_KIMI_CMD", "kimi"),
		KimiHome:  os.Getenv("GE_AGENT_KIMI_HOME"),
		KimiModel: os.Getenv("GE_AGENT_KIMI_MODEL"),
	}
	switch {
	case c.GeMcpPath == "":
		return nil, fmt.Errorf("GE_MCP_PATH not set (path to the ge-mcp binary)")
	case c.GeMcpDSN == "":
		return nil, fmt.Errorf("GE_MCP_DSN not set (passed through to ge-mcp)")
	}
	return c, nil
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
