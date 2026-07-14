// ge-agent: one directive research cycle per invocation. MiniMax-M3 (via its
// Anthropic-compatible API) drives the DIRECTIVE.md loop against ge-mcp's
// tools; the run's durable artifact is one markdown report.
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

	path, err := loop.Run(ctx, cfg)
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
