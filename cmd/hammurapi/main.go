// Command hammurapi is the single Hammurapi binary. The first argument selects
// the mode: api, worker, cleaner or migrate (mcp-proxy is internal: it bridges
// stdio MCP to the api's HTTP MCP endpoint for agents without HTTP MCP support).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/GeenOnGrey/hammurapi-core/internal/app"
	"github.com/GeenOnGrey/hammurapi-core/internal/config"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/logging"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: hammurapi <mode>

modes:
  api       HTTP API, SSE, webhooks and the agent
  worker    Kafka consumer: push events and archive imports
  cleaner   one maintenance pass (run as a CronJob)
  migrate   apply database migrations
  version   print the version`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	mode := os.Args[1]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch mode {
	case "version":
		fmt.Println(version)
		return
	case "mcp-proxy":
		if err := mcp.RunStdioProxy(ctx, os.Getenv("HAMMURAPI_MCP_URL"), os.Getenv("HAMMURAPI_MCP_TOKEN")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case "api", "worker", "cleaner", "migrate":
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err == nil {
		err = cfg.Validate(mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(1)
	}
	slog.SetDefault(logging.New(os.Stdout, cfg.LogLevel).With("mode", mode, "version", version))
	shutdown, err := telemetry.Setup(ctx, cfg.OTLPEndpoint, mode)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}
	defer shutdown(context.Background()) //nolint:errcheck

	run := map[string]func(context.Context, *config.Config) error{
		"api": app.RunAPI, "worker": app.RunWorker, "cleaner": app.RunCleaner, "migrate": app.RunMigrate,
	}[mode]
	slog.Info("starting")
	if err := run(ctx, cfg); err != nil {
		slog.Error("stopped with error", "err", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}
