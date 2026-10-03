package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Local runs tasks as subprocesses of the current binary.
type Local struct {
	Binary  string // path to the hammurapi binary; defaults to os.Executable()
	WorkDir string
	Timeout time.Duration
	// Env is passed to the runner in addition to the task variables: never
	// instance secrets (the runner has no LLM keys either, HMR.CMN-0004).
	Env []string
	// WorkspaceHost is how the agent operator reaches this process's
	// workspace servers (each task listens on a free port).
	WorkspaceHost string

	mu    sync.Mutex
	procs map[string]*localProc
}

type localProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// NewLocal creates a local executor.
func NewLocal(workDir string, timeout time.Duration, env []string) *Local {
	return &Local{WorkDir: workDir, Timeout: timeout, Env: env, procs: map[string]*localProc{}}
}

// Name implements Executor.
func (l *Local) Name() string { return "local" }

// Start implements Executor.
func (l *Local) Start(_ context.Context, t Task) (string, error) {
	bin := l.Binary
	if bin == "" {
		var err error
		if bin, err = os.Executable(); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(l.WorkDir, t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// The task runs detached from the request context, bounded by the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), l.Timeout)
	cmd := exec.CommandContext(ctx, bin, "runner", "--task", t.ID)
	cmd.Dir = dir
	cmd.Env = append(minimalEnv(), l.Env...)
	cmd.Env = append(cmd.Env,
		"HAMMURAPI_TASK_ID="+t.ID, "HAMMURAPI_TASK_TOKEN="+t.Token,
		"HAMMURAPI_INTERNAL_URL="+t.InternalURL, "HAMMURAPI_WORKDIR="+dir, "TRACEPARENT="+t.TraceParent,
		"HAMMURAPI_WORKSPACE_ADDR=:0", "HAMMURAPI_WORKSPACE_HOST="+l.WorkspaceHost)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return "", err
	}
	ref := strconv.Itoa(cmd.Process.Pid)
	p := &localProc{cmd: cmd, done: make(chan struct{})}
	l.mu.Lock()
	l.procs[ref] = p
	l.mu.Unlock()
	go func() {
		p.err = cmd.Wait()
		cancel()
		close(p.done)
		_ = os.RemoveAll(dir)
		slog.Info("runner task exited", "task", t.ID, "pid", ref, "err", p.err)
	}()
	return ref, nil
}

// Stop implements Executor.
func (l *Local) Stop(_ context.Context, ref string) error {
	l.mu.Lock()
	p := l.procs[ref]
	l.mu.Unlock()
	if p == nil {
		return ErrNotFound
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	return p.cmd.Process.Kill()
}

// Status implements Executor.
func (l *Local) Status(_ context.Context, ref string) (Status, error) {
	l.mu.Lock()
	p := l.procs[ref]
	l.mu.Unlock()
	if p == nil {
		// Unknown to this worker (e.g. after a restart): the process is gone.
		return StatusUnknown, nil
	}
	select {
	case <-p.done:
		if p.err != nil {
			return StatusFailed, nil
		}
		return StatusFinished, nil
	default:
		return StatusRunning, nil
	}
}

// minimalEnv keeps only what a process needs to run; secrets of the worker
// (DATABASE_URL, tokens, keys) are not inherited.
func minimalEnv() []string {
	var out []string
	for _, k := range []string{"PATH", "HOME", "LANG", "TZ", "TMPDIR", "SYSTEMROOT", "USERPROFILE", "APPDATA", "LOG_LEVEL", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, fmt.Sprintf("%s=%s", k, v))
		}
	}
	return out
}
