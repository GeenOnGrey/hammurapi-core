package acp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The fake agent binary is built once for the package (arch spec §20).
var fakeAgent string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeagent")
	if err != nil {
		panic(err)
	}
	fakeAgent = filepath.Join(dir, "fakeagent")
	if runtime.GOOS == "windows" {
		fakeAgent += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", fakeAgent, "../../../cmd/fakeagent")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		panic("build fake agent: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newPool(t *testing.T, maxProcs int) *Pool {
	p := NewPool(Config{Command: fakeAgent, MaxProcs: maxProcs, IdleTimeout: time.Minute})
	t.Cleanup(p.Close)
	return p
}

var noMCP = func(AgentCaps) SessionSetup {
	return SessionSetup{MCPServers: []MCPServer{{Type: "http", Name: "hammurapi", URL: "http://127.0.0.1:1/mcp"}}}
}

func prompt(t *testing.T, p *Pool, user uuid.UUID, text string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := p.Ensure(ctx, user, noMCP); err != nil {
		return "", err
	}
	var mu sync.Mutex
	var b strings.Builder
	_, err := p.Prompt(ctx, user, []ContentBlock{TextBlock(text)}, func(u Update) {
		if u.Kind == "token" {
			mu.Lock()
			b.WriteString(u.Text)
			mu.Unlock()
		}
	})
	mu.Lock()
	defer mu.Unlock()
	return b.String(), err
}

// ACP-01 / CHAT-06: initialize + session/new, answer streamed in chunks.
func TestPromptStreamsTokens(t *testing.T) {
	p := newPool(t, 2)
	got, err := prompt(t, p, uuid.New(), "hello agent")
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo: hello agent" {
		t.Fatalf("got %q", got)
	}
	if !p.Caps().HTTPMCP {
		t.Fatal("capabilities from initialize not recorded")
	}
}

// ACP-05: filesystem requests are refused (not declared in capabilities).
func TestFilesystemRefused(t *testing.T) {
	p := newPool(t, 1)
	got, err := prompt(t, p, uuid.New(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "fs refused") {
		t.Fatalf("got %q", got)
	}
}

// ACP-02: a crash fails the running prompt; the next message gets a new session.
func TestCrashRecovery(t *testing.T) {
	p := newPool(t, 1)
	u := uuid.New()
	if _, err := prompt(t, p, u, "crash"); err == nil {
		t.Fatal("expected an error from the crashed agent")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := prompt(t, p, u, "again")
		if err == nil && got == "echo: again" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no recovery: %q %v", got, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ACP-07: with more users than ACP_MAX_PROCESSES, sessions share processes.
func TestProcessLimit(t *testing.T) {
	p := newPool(t, 1)
	for i := 0; i < 3; i++ {
		if _, err := prompt(t, p, uuid.New(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	n := len(p.procs)
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("processes = %d, want 1", n)
	}
}

// ACP-06: idle sessions are closed.
func TestIdleSessionsReaped(t *testing.T) {
	p := newPool(t, 1)
	u := uuid.New()
	closed := make(chan uuid.UUID, 1)
	p.cfg.OnSessionClosed = func(id uuid.UUID) { closed <- id }
	if _, err := prompt(t, p, u, "hi"); err != nil {
		t.Fatal(err)
	}
	p.reap(time.Now().Add(2 * time.Minute))
	select {
	case id := <-closed:
		if id != u {
			t.Fatalf("closed %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session not closed")
	}
}

// CHAT-07: an invalid command yields an error, not a panic.
func TestBadCommand(t *testing.T) {
	p := NewPool(Config{Command: filepath.Join(t.TempDir(), "missing-agent"), MaxProcs: 1, IdleTimeout: time.Minute})
	defer p.Close()
	if _, err := p.Ensure(context.Background(), uuid.New(), noMCP); err == nil {
		t.Fatal("expected error")
	}
	empty := NewPool(Config{MaxProcs: 1})
	defer empty.Close()
	if _, err := empty.Ensure(context.Background(), uuid.New(), noMCP); err != ErrNotConfigured {
		t.Fatalf("got %v", err)
	}
}
