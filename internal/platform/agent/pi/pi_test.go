package pi

import (
	"context"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/fakellm"
)

const testKey = "sk-hmr-test-0123456789"

func testRuntime(t *testing.T) Runtime {
	t.Helper()
	cmd := strings.Fields(os.Getenv("HMR_PI_CMD"))
	if len(cmd) == 0 {
		t.Skip("HMR_PI_CMD is not set (contract tests against a real pi)")
	}
	rt := Runtime{Command: cmd, Options: Options{Path: os.Getenv("PATH"), CWD: t.TempDir()}}
	if runtime.GOOS == "windows" {
		rt.ExtraEnv = []string{"SystemRoot=" + os.Getenv("SystemRoot")}
	}
	return rt
}

func spec(url string, models ...string) agent.ModelSpec {
	high, max := "high", "max"
	m := agent.ModelSpec{Provider: "hmr-test", ConnectionID: "c1", API: "openai-completions", BaseURL: url, ModelID: models[0], Thinking: "off"}
	for _, id := range models {
		m.Models = append(m.Models, agent.ModelDef{ID: id, ContextWindow: 1000000, MaxTokens: 8000, Reasoning: true,
			Input: []string{"text"}, ThinkingLevelMap: map[string]*string{"high": &high, "xhigh": &max},
			Cost: agent.Cost{Input: 1.74, Output: 3.48, CacheRead: 0.145}})
	}
	return m
}

func provider(t *testing.T) (*fakellm.Server, string) {
	f := &fakellm.Server{Key: testKey}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func open(t *testing.T, rt Runtime, req agent.SessionRequest) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := Start(ctx, rt, t.TempDir(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func prompt(t *testing.T, s *Session, text string) (string, []agent.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var b strings.Builder
	var evs []agent.Event
	if err := s.Prompt(ctx, agent.PromptRequest{Text: text}, func(e agent.Event) {
		evs = append(evs, e)
		if e.Type == agent.EventTextDelta {
			b.WriteString(e.Delta)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return b.String(), evs
}

func last(evs []agent.Event) agent.Event { return evs[len(evs)-1] }

func find(evs []agent.Event, typ string) *agent.Event {
	for i := range evs {
		if evs[i].Type == typ {
			return &evs[i]
		}
	}
	return nil
}

func chatReq(url string, models ...string) agent.SessionRequest {
	return agent.SessionRequest{Scenario: agent.ScenarioChat, Kind: agent.KindChat, Model: spec(url, models...),
		Secrets: agent.Secrets{LLMKey: testKey}, SystemAppend: "You are Hammurapi's agent."}
}

// PI-04, PI-05, PI-06: a chat session declares no built-in tools, the process
// environment is clean and the files hold no secrets.
func TestChatSessionIsolation(t *testing.T) {
	rt := testRuntime(t)
	f, url := provider(t)
	s := open(t, rt, chatReq(url, "ok"))
	text, evs := prompt(t, s, "hello")
	if text != "echo: hello" || last(evs).Type != agent.EventSettled {
		t.Fatalf("text %q events %+v", text, evs)
	}
	u := find(evs, agent.EventUsage)
	if u == nil || u.TokensIn+u.CacheRead == 0 || u.CostUSD <= 0 {
		t.Fatalf("usage %+v", u)
	}
	for _, r := range f.Requests() {
		if n := r.ToolNames(); len(n) > 0 {
			t.Fatalf("tools declared in a chat session: %v", n)
		}
	}
	_ = filepath.WalkDir(s.layout.AgentDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), testKey) {
				t.Errorf("%s contains the LLM key", p)
			}
		}
		return nil
	})
	for _, kv := range Env(s.layout, s.req, s.rt.Options) {
		k := kv[:strings.IndexByte(kv, '=')]
		ok := k == "PATH" || k == "HOME" || k == "LANG" || k == "SystemRoot" || strings.HasPrefix(k, "HMR_") || strings.HasPrefix(k, "PI_")
		if !ok {
			t.Errorf("unexpected variable %s in the Pi environment", k)
		}
	}
}

// ERR-01, ERR-02: balance and auth errors are classified and not retried.
func TestProviderErrors(t *testing.T) {
	rt := testRuntime(t)
	_, url := provider(t)
	for model, class := range map[string]agent.ErrorClass{"e402": agent.ErrInsufficientBalance, "e401": agent.ErrAuth} {
		s := open(t, rt, chatReq(url, model))
		_, evs := prompt(t, s, "hi")
		e := last(evs)
		if e.Type != agent.EventError || e.ErrorClass != class || find(evs, agent.EventRetry) != nil {
			t.Fatalf("%s: %+v", model, evs)
		}
	}
}

// ERR-03: two 429 then success — retries are visible as events, the answer arrives.
func TestRateLimitRetried(t *testing.T) {
	rt := testRuntime(t)
	_, url := provider(t)
	s := open(t, rt, chatReq(url, "flaky"))
	text, evs := prompt(t, s, "hi")
	if text != "echo: hi" || last(evs).Type != agent.EventSettled || find(evs, agent.EventRetry) == nil {
		t.Fatalf("%q %+v", text, evs)
	}
}

// ERR-05: context overflow → compaction and one retry.
func TestContextOverflowCompactsAndRetries(t *testing.T) {
	rt := testRuntime(t)
	_, url := provider(t)
	s := open(t, rt, chatReq(url, "ok", "ctxonce"))
	// Pi compacts only what is older than the recent ~20k tokens: give it enough history.
	big := strings.Repeat("lorem ipsum dolor sit amet ", 2500)
	prompt(t, s, "first: "+big)
	prompt(t, s, "second: "+big)
	if err := s.SetModel(context.Background(), "ctxonce", ""); err != nil {
		t.Fatal(err)
	}
	text, evs := prompt(t, s, "third")
	if last(evs).Type != agent.EventSettled || find(evs, agent.EventCompaction) == nil || !strings.HasPrefix(text, "echo:") {
		t.Fatalf("%q %+v", text, evs)
	}
}

// PI-08: a session restored from its snapshot continues the conversation.
func TestSnapshotRestore(t *testing.T) {
	rt := testRuntime(t)
	f, url := provider(t)
	s := open(t, rt, chatReq(url, "ok"))
	prompt(t, s, "remember the word apricot")
	snap, err := s.Snapshot(context.Background())
	if err != nil || len(snap) == 0 {
		t.Fatalf("snapshot %v", err)
	}
	// The first process and its directory are gone, as after an idle close.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	req := chatReq(url, "ok")
	req.Snapshot = snap
	s2 := open(t, rt, req)
	prompt(t, s2, "which word?")
	rs := f.Requests()
	lastReq := rs[len(rs)-1]
	found := false
	for _, m := range lastReq.Messages {
		found = found || strings.Contains(m.Text(), "apricot")
	}
	if !found {
		t.Fatal("the restored session lost the earlier message")
	}
}

// PI-09 (unit part): without a snapshot the chat history seeds the first prompt.
func TestHistorySeedsFirstPrompt(t *testing.T) {
	rt := testRuntime(t)
	f, url := provider(t)
	req := chatReq(url, "ok")
	req.History = []agent.HistoryMessage{{Role: "user", Text: "my name is Lena"}, {Role: "assistant", Text: "Hello Lena"}}
	s := open(t, rt, req)
	prompt(t, s, "what is my name?")
	rs := f.Requests()
	if !strings.Contains(rs[len(rs)-1].LastUser(), "my name is Lena") {
		t.Fatalf("history not sent: %q", rs[len(rs)-1].LastUser())
	}
}

// CON-04 (I part): a check with a wrong key reports auth for every model.
func TestCheckLLM(t *testing.T) {
	rt := testRuntime(t)
	_, url := provider(t)
	ok := CheckLLM(context.Background(), rt, t.TempDir(), agent.LLMCheckRequest{Model: spec(url, "ok", "e402"), Secrets: agent.Secrets{LLMKey: testKey}})
	if len(ok.Results) != 2 || !ok.Results[0].OK || ok.Results[1].ErrorClass != agent.ErrInsufficientBalance {
		t.Fatalf("%+v", ok.Results)
	}
	bad := CheckLLM(context.Background(), rt, t.TempDir(), agent.LLMCheckRequest{Model: spec(url, "ok"), Secrets: agent.Secrets{LLMKey: "sk-wrong-key-123"}})
	if bad.Results[0].OK || bad.Results[0].ErrorClass != agent.ErrAuth {
		t.Fatalf("%+v", bad.Results)
	}
}
