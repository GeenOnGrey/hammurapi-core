package runner

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/fakellm"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/pi"
)

// RUN-02, RUN-03 (K): the hammurapi-workspace extension runs Pi's built-in
// tools in the runner's working copy. Requires HMR_PI_CMD (a real pi).
func TestExtensionRoutesToolsToWorkspace(t *testing.T) {
	cmd := strings.Fields(os.Getenv("HMR_PI_CMD"))
	if len(cmd) == 0 {
		t.Skip("HMR_PI_CMD is not set")
	}
	root := t.TempDir()
	ws := &Workspace{Root: root, Token: "ws-contract-token", Env: CommandEnv(root)}
	wsrv := httptest.NewServer(ws.Handler())
	defer wsrv.Close()

	steps := []fakellm.ToolCall{
		{Name: "write", Args: map[string]any{"path": "notes/a.txt", "content": "hello from the agent\n"}},
		{Name: "read", Args: map[string]any{"path": "notes/a.txt"}},
		{Name: "bash", Args: map[string]any{"command": "echo done> b.txt"}},
		{Name: "grep", Args: map[string]any{"pattern": "hello"}},
		{Name: "edit", Args: map[string]any{"path": "notes/a.txt", "edits": []map[string]string{{"oldText": "hello", "newText": "hi"}}}},
		{Name: "ls", Args: map[string]any{"path": "notes"}},
	}
	llm := &fakellm.Server{Key: "sk-k", Script: func(r fakellm.Request) *fakellm.Reply {
		if n := r.ToolResults(); n < len(steps) {
			return &fakellm.Reply{ToolCalls: []fakellm.ToolCall{steps[n]}}
		}
		return &fakellm.Reply{Text: "finished"}
	}}
	lsrv := httptest.NewServer(llm)
	defer lsrv.Close()

	ext, _ := filepath.Abs(filepath.Join("..", "..", "..", "pi-extensions", "hammurapi-workspace"))
	rt := pi.Runtime{Command: cmd, Options: pi.Options{Path: os.Getenv("PATH"), ExtensionDir: ext}}
	if runtime.GOOS == "windows" {
		rt.ExtraEnv = []string{"SystemRoot=" + os.Getenv("SystemRoot")}
	}
	req := agent.SessionRequest{Scenario: agent.ScenarioCodegen, Kind: agent.KindTask,
		Model: agent.ModelSpec{Provider: "hmr-k", API: "openai-completions", BaseURL: lsrv.URL, ModelID: "m", Thinking: "off",
			Models: []agent.ModelDef{{ID: "m", ContextWindow: 100000, MaxTokens: 8000, Input: []string{"text"}}}},
		Secrets:   agent.Secrets{LLMKey: "sk-k"},
		Workspace: &agent.Workspace{URL: wsrv.URL, Token: ws.Token}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := pi.Start(ctx, rt, t.TempDir(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var results []agent.Event
	if err := s.Prompt(ctx, agent.PromptRequest{Text: "do the task"}, func(e agent.Event) {
		if e.Type == agent.EventToolResult {
			results = append(results, e)
		}
		if e.Type == agent.EventError {
			t.Errorf("error event %+v", e)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(results) != len(steps) {
		t.Fatalf("tool results %d: %+v", len(results), results)
	}
	for i, r := range results {
		if r.IsError {
			t.Errorf("%s failed: %s", steps[i].Name, r.Summary)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "notes", "a.txt")); string(b) != "hi from the agent\n" {
		t.Fatalf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "b.txt")); !strings.HasPrefix(string(b), "done") {
		t.Fatalf("bash did not run in the workspace: %q", b)
	}
	if !strings.Contains(results[1].Summary, "hello from the agent") || !strings.Contains(results[3].Summary, "notes/a.txt:1: hello from the agent") ||
		!strings.Contains(results[5].Summary, "a.txt") {
		t.Fatalf("results: %+v", results)
	}
	// The tools declared to the model are Pi's built-in names.
	reqs := llm.Requests()
	names := strings.Join(reqs[0].ToolNames(), ",")
	for _, n := range []string{"read", "write", "edit", "bash", "grep", "find", "ls"} {
		if !strings.Contains(","+names+",", ","+n+",") {
			t.Fatalf("tool %s not declared: %s", n, names)
		}
	}
}
