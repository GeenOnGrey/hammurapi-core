// Package executor starts runner tasks (PLT.HMR-0002 arch §7.3):
//
//	local  a subprocess `hammurapi runner --task <id>` of the worker, with a
//	       working directory under RUNNER_WORKDIR (Docker Compose, development)
//	k8s    a Kubernetes Job per task in a separate namespace, created through
//	       the Kubernetes REST API with the worker's service account
//
// A task receives only its id, a one-time task token and the internal API URL;
// instance secrets are never passed to it.
package executor

//go:generate go tool mockgen -destination=mocks/executor.go -package=mocks . Executor

import (
	"context"
	"errors"
)

// Task is what an executor needs to start a runner.
type Task struct {
	ID          string
	Token       string // one-time task token for the internal API
	InternalURL string // e.g. http://hammurapi-internal:8081
	TraceParent string // W3C trace context of the workflow run
}

// Status of a started task as seen by the executor.
type Status string

const (
	StatusRunning  Status = "running"
	StatusFinished Status = "finished"
	StatusFailed   Status = "failed"
	StatusUnknown  Status = "unknown"
)

// Executor starts and stops runner tasks.
type Executor interface {
	Name() string
	// Start launches the task and returns a reference (PID or Job name).
	Start(ctx context.Context, t Task) (string, error)
	// Stop cancels a running task.
	Stop(ctx context.Context, ref string) error
	// Status reports whether the task process is still alive (for the cleaner).
	Status(ctx context.Context, ref string) (Status, error)
}

// ErrNotFound is returned for unknown references.
var ErrNotFound = errors.New("task not found")
