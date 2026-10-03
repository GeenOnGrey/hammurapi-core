package operator

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/pi"
)

// The test binary doubles as a fake Pi (scripted RPC): the operator starts it
// with OPERATOR_FAKE_PI=1 in the session environment.
func TestMain(m *testing.M) {
	if os.Getenv("OPERATOR_FAKE_PI") == "1" {
		fakePi()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakePi() {
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	var mu sync.Mutex
	emit := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		out.Write(append(b, '\n'))
		out.Flush()
		mu.Unlock()
	}
	ok := func(id, cmd string, data any) {
		emit(map[string]any{"id": id, "type": "response", "command": cmd, "success": true, "data": data})
	}
	sessionDir := ""
	for i, a := range os.Args {
		if a == "--session-dir" && i+1 < len(os.Args) {
			sessionDir = os.Args[i+1]
		}
	}
	file := filepath.Join(sessionDir, "fake.jsonl")
	model := ""
	tokens := 0
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var c struct {
			ID, Type, ModelID, Message string
		}
		_ = json.Unmarshal(line, &c)
		switch c.Type {
		case "set_model":
			model = c.ModelID
			ok(c.ID, c.Type, map[string]any{"id": model, "provider": "hmr-x"})
		case "get_state":
			ok(c.ID, c.Type, map[string]any{"sessionFile": file, "thinkingLevel": "off"})
		case "get_session_stats":
			ok(c.ID, c.Type, map[string]any{"tokens": map[string]int{"input": tokens, "output": tokens / 10}, "cost": float64(tokens) / 1e6})
		case "abort":
			ok(c.ID, c.Type, nil)
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "aborted"}})
			emit(map[string]any{"type": "agent_end", "willRetry": false})
			emit(map[string]any{"type": "agent_settled"})
		case "prompt":
			ok(c.ID, c.Type, map[string]string{"disposition": "started"})
			_ = os.WriteFile(file, []byte(`{"type":"session"}`+"\n"), 0o600)
			if model == "hang" { // runs until abort
				emit(map[string]any{"type": "agent_start"})
				emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "partial"}})
				continue
			}
			if model == "slow" {
				time.Sleep(700 * time.Millisecond)
			}
			tokens += 100
			emit(map[string]any{"type": "agent_start"})
			emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "echo: " + c.Message}})
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop"}})
			emit(map[string]any{"type": "agent_end", "willRetry": false})
			emit(map[string]any{"type": "agent_settled"})
		default:
			ok(c.ID, c.Type, nil)
		}
	}
}

const svc = "service-token-123"

func newOperator(t *testing.T, mutate func(*Config)) (*Operator, *httptest.Server) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Runtime: pi.Runtime{Command: []string{exe}, Options: pi.Options{Path: os.Getenv("PATH"),
		ExtraEnv: []string{"OPERATOR_FAKE_PI=1", "SystemRoot=" + os.Getenv("SystemRoot")}}},
		WorkDir: t.TempDir(), ServiceToken: svc, MaxSessions: 2, MaxTaskSessions: 1}
	if mutate != nil {
		mutate(&cfg)
	}
	o, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(o.Handler())
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		o.Run(ctx)
	})
	return o, srv
}

func sreq(scenario agent.Scenario, model string) agent.SessionRequest {
	r := agent.SessionRequest{Scenario: scenario, Model: agent.ModelSpec{Provider: "hmr-x", API: "openai-completions",
		BaseURL: "http://127.0.0.1:1", ModelID: model, Models: []agent.ModelDef{{ID: model}}}, Secrets: agent.Secrets{LLMKey: "sk-test-key"}}
	if scenario.HasCode() {
		r.Workspace = &agent.Workspace{URL: "http://10.0.0.5:8095", Token: "ws-token-1"}
	}
	return r
}

func client(srv *httptest.Server, tok string) *agent.Client {
	return &agent.Client{BaseURL: srv.URL, Token: tok}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestPromptStream(t *testing.T) {
	_, srv := newOperator(t, nil)
	c := client(srv, svc)
	s, err := c.Open(ctx(t), sreq(agent.ScenarioChat, "ok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var types []string
	err = c.Prompt(ctx(t), s.SessionID, agent.PromptRequest{Text: "hi"}, func(e agent.Event) {
		types = append(types, e.Type)
		text += e.Delta
	})
	if err != nil || text != "echo: hi" || types[len(types)-1] != agent.EventSettled {
		t.Fatalf("%q %v %v", text, types, err)
	}
	if !contains(types, agent.EventUsage) {
		t.Fatalf("no usage event: %v", types)
	}
	snap, err := c.Snapshot(ctx(t), s.SessionID)
	if err != nil || !strings.Contains(string(snap), "session") {
		t.Fatalf("snapshot %q %v", snap, err)
	}
	if err := c.Close(ctx(t), s.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := c.Prompt(ctx(t), s.SessionID, agent.PromptRequest{Text: "x"}, func(agent.Event) {}); !errors.Is(err, agent.ErrSessionGone) {
		t.Fatalf("closed session: %v", err)
	}
}

// PI-11: the service token, the session token and a foreign session token.
// An aborted run ends the stream with settled{aborted}, not a broken stream:
// callers must not take it for a lost session and resend the prompt.
func TestAbortSettles(t *testing.T) {
	_, srv := newOperator(t, nil)
	c := client(srv, svc)
	s, err := c.Open(ctx(t), sreq(agent.ScenarioChat, "hang"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var last agent.Event
	done := make(chan error, 1)
	started := make(chan struct{}, 1)
	go func() {
		done <- c.Prompt(ctx(t), s.SessionID, agent.PromptRequest{Text: "long"}, func(e agent.Event) {
			if e.Type == agent.EventTextDelta {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			last = e
		})
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not start")
	}
	if err := c.Abort(ctx(t), s.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("aborted run: %v", err)
	}
	if last.Type != agent.EventSettled || !last.Aborted {
		t.Fatalf("last event %+v", last)
	}
}

func TestAuth(t *testing.T) {
	_, srv := newOperator(t, func(c *Config) { c.MaxTaskSessions = 2 })
	c := client(srv, svc)
	a, err := c.Open(ctx(t), sreq(agent.ScenarioCodegen, "ok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Open(ctx(t), sreq(agent.ScenarioCodegen, "ok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var ae *agent.APIError
	if _, err := client(srv, "wrong").Open(ctx(t), sreq(agent.ScenarioChat, "ok"), nil); !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("wrong service token: %v", err)
	}
	if _, err := client(srv, a.SessionToken).Open(ctx(t), sreq(agent.ScenarioChat, "ok"), nil); !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("session token opened a session: %v", err)
	}
	// The runner of session a: its own session works, session b does not.
	ra := client(srv, a.SessionToken)
	if err := ra.Prompt(ctx(t), a.SessionID, agent.PromptRequest{Text: "x"}, func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := ra.Prompt(ctx(t), b.SessionID, agent.PromptRequest{Text: "x"}, func(agent.Event) {}); !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("foreign session: %v", err)
	}
	if _, err := ra.Snapshot(ctx(t), a.SessionID); !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("session token got a snapshot: %v", err)
	}
	if err := ra.Close(ctx(t), a.SessionID); err != nil {
		t.Fatal(err)
	}
}

// PI-10, RUN-08: chat and task sessions have separate limits.
func TestLimits(t *testing.T) {
	_, srv := newOperator(t, func(c *Config) { c.MaxSessions = 1; c.MaxTaskSessions = 1 })
	c := client(srv, svc)
	chat, err := c.Open(ctx(t), sreq(agent.ScenarioChat, "ok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var be *agent.BusyError
	if _, err := c.Open(ctx(t), sreq(agent.ScenarioIssueAnalysis, "ok"), nil); !errors.As(err, &be) || be.RetryAfter <= 0 {
		t.Fatalf("over the limit: %v", err)
	}
	task, err := c.Open(ctx(t), sreq(agent.ScenarioCodegen, "ok"), nil)
	if err != nil {
		t.Fatalf("task session blocked by chat sessions: %v", err)
	}
	if _, err := c.Open(ctx(t), sreq(agent.ScenarioReviewUpdate, "ok"), nil); !errors.As(err, &be) {
		t.Fatalf("task limit: %v", err)
	}
	_ = c.Close(ctx(t), chat.SessionID)
	if _, err := c.Open(ctx(t), sreq(agent.ScenarioChat, "ok"), nil); err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
	_ = c.Close(ctx(t), task.SessionID)
}

func TestValidationAndBusySession(t *testing.T) {
	_, srv := newOperator(t, nil)
	c := client(srv, svc)
	bad := sreq(agent.ScenarioChat, "ok")
	bad.Workspace = &agent.Workspace{URL: "http://x", Token: "t"}
	var ae *agent.APIError
	if _, err := c.Open(ctx(t), bad, nil); !errors.As(err, &ae) || ae.Status != 422 {
		t.Fatalf("workspace in chat: %v", err)
	}
	noWS := sreq(agent.ScenarioCodegen, "ok")
	noWS.Workspace = nil
	if _, err := c.Open(ctx(t), noWS, nil); !errors.As(err, &ae) || ae.Status != 422 {
		t.Fatalf("codegen without workspace: %v", err)
	}
	s, err := c.Open(ctx(t), sreq(agent.ScenarioChat, "slow"), nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Prompt(ctx(t), s.SessionID, agent.PromptRequest{Text: "1"}, func(agent.Event) {}) }()
	time.Sleep(200 * time.Millisecond)
	if err := c.Prompt(ctx(t), s.SessionID, agent.PromptRequest{Text: "2"}, func(agent.Event) {}); !errors.As(err, &ae) || ae.Code != "session_busy" {
		t.Fatalf("concurrent prompt: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func bundle(t *testing.T, files map[string]string) ([]byte, string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:])
}

// SK-10: an unknown snapshot is requested once, then served from the cache.
func TestSkillsBundle(t *testing.T) {
	o, srv := newOperator(t, nil)
	c := client(srv, svc)
	b, hash := bundle(t, map[string]string{"spec-review/SKILL.md": "---\nname: spec-review\ndescription: Review\n---\nBody"})
	req := sreq(agent.ScenarioChat, "ok")
	req.Skills = &agent.Skills{Hash: hash, Names: []string{"spec-review"}}
	var ae *agent.APIError
	if _, err := c.Open(ctx(t), req, nil); !errors.As(err, &ae) || ae.Code != "skills_bundle_required" {
		t.Fatalf("unknown hash: %v", err)
	}
	asked := 0
	s, err := c.Open(ctx(t), req, func(context.Context) ([]byte, error) { asked++; return b, nil })
	if err != nil || asked != 1 {
		t.Fatalf("open with bundle: %v asked=%d", err, asked)
	}
	_ = c.Close(ctx(t), s.SessionID)
	if _, err := os.Stat(filepath.Join(o.skills.dir, strings.TrimPrefix(hash, "sha256:"), "spec-review", "SKILL.md")); err != nil {
		t.Fatalf("snapshot not cached: %v", err)
	}
	s, err = c.Open(ctx(t), req, func(context.Context) ([]byte, error) { asked++; return b, nil })
	if err != nil || asked != 1 {
		t.Fatalf("cached hash asked again: %v asked=%d", err, asked)
	}
	_ = c.Close(ctx(t), s.SessionID)

	// A bundle that does not match its hash, and one with a path escape.
	req.Skills = &agent.Skills{Hash: "sha256:" + strings.Repeat("0", 64), Names: []string{"x"}, Bundle: b}
	if _, err := c.Open(ctx(t), req, nil); !errors.As(err, &ae) || ae.Status != 422 {
		t.Fatalf("hash mismatch: %v", err)
	}
	evil, eh := bundle(t, map[string]string{"../evil/SKILL.md": "x"})
	req.Skills = &agent.Skills{Hash: eh, Names: []string{"evil"}, Bundle: evil}
	if _, err := c.Open(ctx(t), req, nil); !errors.As(err, &ae) || ae.Status != 422 {
		t.Fatalf("path escape: %v", err)
	}
}

func TestIdleSessionsClosed(t *testing.T) {
	o, srv := newOperator(t, func(c *Config) { c.IdleTimeout = 50 * time.Millisecond })
	c := client(srv, svc)
	s, err := c.Open(ctx(t), sreq(agent.ScenarioChat, "ok"), nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	o.reap()
	if o.get(s.SessionID) != nil {
		t.Fatal("idle session not closed")
	}
	if _, err := os.Stat(filepath.Join(o.cfg.WorkDir, "sessions", s.SessionID)); !os.IsNotExist(err) {
		t.Fatalf("session directory left: %v", err)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
