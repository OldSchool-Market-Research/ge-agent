// ge-agent: one directive research cycle per invocation. A model drives the
// DIRECTIVE.md loop against ge-mcp's tools; the run's durable artifact is
// one markdown report. GE_AGENT_DRIVER picks the model loop: "api" (MiniMax-M3
// via its Anthropic-compatible API) or "kimi" (the Kimi Code CLI, with this
// binary re-run as its MCP proxy: -mcp-proxy).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/osrs-ge/ge-agent/internal/config"
	"github.com/osrs-ge/ge-agent/internal/loop"
	"github.com/osrs-ge/ge-agent/internal/mcpbridge"
)

func main() {
	log.SetPrefix("ge-agent: ")
	listTools := flag.Bool("list-tools", false, "connect to ge-mcp, print its tools as Anthropic definitions, exit")
	mcpProxy := flag.Bool("mcp-proxy", false, "serve ge-mcp's tools plus submit_report over MCP stdio (spawned by the kimi driver)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	if *listTools {
		if err := printTools(ctx, cfg); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *mcpProxy {
		if err := loop.ServeProxy(ctx, cfg); err != nil {
			log.Fatal(err)
		}
		return
	}

	run := loop.Run
	if cfg.Driver == "kimi" {
		run = loop.RunKimi
	}
	path, err := run(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(path)
}

func printTools(ctx context.Context, cfg *config.Config) error {
	bridge, err := mcpbridge.New(ctx, cfg.GeMcpPath, cfg.GeMcpDSN)
	if err != nil {
		return err
	}
	defer bridge.Close()
	tools, err := bridge.Tools(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(tools)
}
