// Command hammurapi is the single Hammurapi binary. The first argument selects
// the mode: api, worker, agent (the agent operator, FTR.HMR.CMN-0004), cleaner,
// migrate or runner (one agent task; started by the worker's executor).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/app"
	"github.com/GreenOnGrey/hammurapi-core/internal/config"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/runner"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/operator"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/pi"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/logging"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: hammurapi <mode>

modes:
  api       HTTP API, SSE, webhooks, the chat and the internal API (:8081)
  worker    webhook events, imports, workflows (state machines) and runner tasks
  agent     the agent operator: Pi sessions behind the internal API (:8090)
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
	case "agent":
		if err := runOperator(ctx); err != nil {
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
		WorkDir:       envOr("HAMMURAPI_WORKDIR", "."),
		WorkspaceAddr: envOr("HAMMURAPI_WORKSPACE_ADDR", ":8095"), WorkspaceHost: os.Getenv("HAMMURAPI_WORKSPACE_HOST"),
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

// runOperator runs the agent operator (FTR.HMR.CMN-0004 arch §3). Its
// configuration comes from its own environment only: the pod has no database,
// Kafka, object storage or Hammurapi secrets.
func runOperator(ctx context.Context) error {
	slog.SetDefault(logging.New(os.Stdout, envOr("LOG_LEVEL", "info")).With("mode", "agent", "version", version))
	maxS, err := strconv.Atoi(envOr("AGENT_MAX_SESSIONS", "20"))
	if err != nil {
		return fmt.Errorf("AGENT_MAX_SESSIONS: %w", err)
	}
	maxT, err := strconv.Atoi(envOr("AGENT_MAX_TASK_SESSIONS", "4"))
	if err != nil {
		return fmt.Errorf("AGENT_MAX_TASK_SESSIONS: %w", err)
	}
	idle, err := time.ParseDuration(envOr("AGENT_IDLE_TIMEOUT", "15m"))
	if err != nil {
		return fmt.Errorf("AGENT_IDLE_TIMEOUT: %w", err)
	}
	op, err := operator.New(operator.Config{
		Runtime: pi.Runtime{Command: strings.Fields(envOr("PI_BINARY", "/usr/local/bin/pi")), Options: pi.Options{
			ExtensionDir: envOr("PI_EXTENSION_DIR", "/opt/hammurapi/pi-extensions/hammurapi-workspace"),
			Path:         envOr("PATH", "/usr/local/bin:/usr/bin:/bin"), Lang: os.Getenv("LANG"),
			// Proxy settings and the like for the Pi processes, never secrets.
			ExtraEnv: strings.Fields(os.Getenv("PI_EXTRA_ENV")),
		}},
		WorkDir: envOr("AGENT_WORKDIR", "/work"), ServiceToken: os.Getenv("AGENT_SERVICE_TOKEN"),
		MaxSessions: maxS, MaxTaskSessions: maxT, IdleTimeout: idle,
	})
	if err != nil {
		return err
	}
	return app.RunOperator(ctx, op, envOr("AGENT_LISTEN_ADDR", ":8090"), envOr("SERVICE_ADDR", ":9100"),
		strings.Fields(envOr("PI_BINARY", "/usr/local/bin/pi")))
}
