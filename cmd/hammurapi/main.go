// Command hammurapi is the single Hammurapi binary. The first argument selects
// the mode: api, worker, cleaner, migrate or runner (one agent task; started by
// the worker's executor). mcp-proxy is internal: it bridges stdio MCP to an
// HTTP MCP endpoint for agents without HTTP MCP support.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/GeenOnGrey/hammurapi-core/internal/app"
	"github.com/GeenOnGrey/hammurapi-core/internal/config"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/runner"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/logging"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: hammurapi <mode>

modes:
  api       HTTP API, SSE, webhooks, the chat agent and the internal API (:8081)
  worker    webhook events, imports, workflows (state machines) and runner tasks
  runner    one agent task: hammurapi runner --task <id>
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
	case "runner":
		if err := runTask(ctx, os.Args[2:]); err != nil {
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

// runTask runs one runner task. The executor passes the task id, a one-time
// task token and the internal API URL; instance secrets are not available here.
func runTask(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	task := fs.String("task", os.Getenv("HAMMURAPI_TASK_ID"), "task id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logging.New(os.Stdout, envOr("LOG_LEVEL", "info")).With("mode", "runner", "task", *task, "version", version))
	cfg := runner.Config{
		TaskID: *task, Token: os.Getenv("HAMMURAPI_TASK_TOKEN"), InternalURL: os.Getenv("HAMMURAPI_INTERNAL_URL"),
		WorkDir: envOr("HAMMURAPI_WORKDIR", "."), ACPCommand: os.Getenv("ACP_AGENT_COMMAND"),
		ACPArgs: strings.Fields(os.Getenv("ACP_AGENT_ARGS")), ACPEnv: splitEnv(os.Getenv("ACP_AGENT_ENV")),
		NewProvider: func(d *runner.Description) git.Provider {
			if d.Provider == "gitlab" {
				return git.NewGitLab(d.GitBaseURL, d.GitBaseURL, d.Repo, "", "")
			}
			return git.NewGitHub(d.GitBaseURL, d.GitBaseURL, d.Repo, "", "")
		},
	}
	if cfg.TaskID == "" || cfg.Token == "" || cfg.InternalURL == "" {
		return fmt.Errorf("runner: --task, HAMMURAPI_TASK_TOKEN and HAMMURAPI_INTERNAL_URL are required")
	}
	return runner.Run(ctx, cfg)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitEnv(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ";") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
