//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agentcfg"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/attachments"
	agentapi "github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/fakellm"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/operator"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/pi"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// capture records published events.
type capture struct {
	mu  sync.Mutex
	evs []events.Event
}

func (c *capture) Publish(_ context.Context, e events.Event) {
	c.mu.Lock()
	c.evs = append(c.evs, e)
	c.mu.Unlock()
}

func (c *capture) wait(t *testing.T, typ string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, e := range c.evs {
			if e.Type != typ {
				continue
			}
			b, _ := json.Marshal(e.Data)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if pred == nil || pred(m) {
				c.mu.Unlock()
				return m
			}
		}
		c.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	c.mu.Lock()
	for _, e := range c.evs {
		b, _ := json.Marshal(e.Data)
		t.Logf("event %s %s", e.Type, b)
	}
	c.mu.Unlock()
	t.Fatalf("no %s event", typ)
	return nil
}

func (c *capture) reset() {
	c.mu.Lock()
	c.evs = nil
	c.mu.Unlock()
}

// The whole chat path with a real Pi (HMR_PI_CMD): chat → operator → pi →
// fake LLM, with Hammurapi's MCP server (PI-08, MOD-04, ERR-01, ERR-08, ERR-09, USE-01).
func TestChatThroughOperator(t *testing.T) {
	cmd := strings.Fields(os.Getenv("HMR_PI_CMD"))
	if len(cmd) == 0 {
		t.Skip("HMR_PI_CMD is not set")
	}
	ctx := context.Background()
	cfgSvc, admin := agentSvc(t)

	llm := &fakellm.Server{}
	lsrv := httptest.NewServer(llm)
	defer lsrv.Close()

	rt := pi.Runtime{Command: cmd, Options: pi.Options{Path: os.Getenv("PATH")}}
	if runtime.GOOS == "windows" {
		rt.ExtraEnv = []string{"SystemRoot=" + os.Getenv("SystemRoot")}
	}
	op, err := operator.New(operator.Config{Runtime: rt, WorkDir: t.TempDir(), ServiceToken: "svc"})
	must(t, err)
	t.Cleanup(func() { // stop the Pi processes before their directories are removed
		c, cancel := context.WithCancel(context.Background())
		cancel()
		op.Run(c)
	})
	osrv := httptest.NewServer(op.Handler())
	defer osrv.Close()
	client := &agentapi.Client{BaseURL: osrv.URL, Token: "svc"}

	// A DeepSeek connection pointed at the fake provider, with an extra model "e402".
	c, err := cfgSvc.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Type: agentcfg.TypeDeepSeek, APIKey: "sk-chat-test-0001"})
	must(t, err)
	models := append(c.Models, agentapi.ModelDef{ID: "e402", ContextWindow: 100000, MaxTokens: 8000, Input: []string{"text"}})
	mj, _ := json.Marshal(models)
	_, err = pool.Exec(ctx, `UPDATE llm_connections SET base_url = $2, models = $3 WHERE id = $1`, c.ID, lsrv.URL, mj)
	must(t, err)
	_, err = cfgSvc.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: c.ID, Model: "deepseek-v4-flash"}})
	must(t, err)

	mcpServer := mcp.NewServer()
	msrv := httptest.NewServer(mcpServer)
	defer msrv.Close()
	hub := &capture{}
	objects := &memStorage{}
	chat := agent.NewService(agent.NewRepository(pool), specdata.NewPG(pool), client, cfgSvc, mcpServer, msrv.URL,
		hub, attachments.NewService(pool, nil, 1<<20, nil), objects, nil)
	user := &domain.Principal{UserID: admin.UserID, GlobalAdmin: true}

	send := func(text string) uuid.UUID {
		t.Helper()
		acc, err := chat.Send(ctx, user, agent.SendInput{Text: text, Mode: "general"})
		must(t, err)
		return acc.MessageID
	}
	done := func(id uuid.UUID) map[string]any {
		return hub.wait(t, events.AgentDone, func(m map[string]any) bool { return m["messageId"] == id.String() })
	}

	// 1. An answer of the default model, stored with the model.
	id := send("remember the word apricot")
	d := done(id)
	if !strings.HasPrefix(d["content"].(string), "echo:") || d["model"] != "deepseek-v4-flash" {
		t.Fatalf("done %v", d)
	}
	info, err := chat.Session(ctx)
	must(t, err)
	if info.Model != "deepseek-v4-flash" || info.ConnectionName != "DeepSeek" {
		t.Fatalf("session %+v", info)
	}

	// 2. MOD-04: the default model changes — the next message switches the model.
	hub.reset()
	_, err = cfgSvc.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: c.ID, Model: "deepseek-v4-pro"}})
	must(t, err)
	id = send("second")
	done(id)
	hub.wait(t, agent.EventChatModel, func(m map[string]any) bool { return m["model"] == "deepseek-v4-pro" })
	rs := llm.Requests()
	if rs[len(rs)-1].Model != "deepseek-v4-pro" {
		t.Fatalf("model of the request: %s", rs[len(rs)-1].Model)
	}

	// 3. ERR-01, ERR-09: 402 → chat.error with the class, the message is marked, focus shows the problem.
	hub.reset()
	_, err = cfgSvc.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: c.ID, Model: "e402"}})
	must(t, err)
	failed := send("third")
	e := hub.wait(t, agent.EventChatError, func(m map[string]any) bool { return m["messageId"] == failed.String() })
	if e["errorClass"] != "insufficient_balance" || e["connectionName"] != "DeepSeek" {
		t.Fatalf("chat.error %v", e)
	}
	items, err := cfgSvc.Focus(ctx)
	must(t, err)
	if len(items) != 1 || items[0].Action != "connection_problem" {
		t.Fatalf("focus %+v", items)
	}

	// 4. ERR-08: after the fix, "retry" repeats the message as a new one.
	hub.reset()
	_, err = cfgSvc.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: c.ID, Model: "deepseek-v4-flash"}})
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_connections SET status_at = now() - interval '2 minutes'`)
	must(t, err)
	acc, err := chat.Retry(ctx, user, failed)
	must(t, err)
	done(acc.MessageID)
	var retryOf *uuid.UUID
	must(t, pool.QueryRow(ctx, `SELECT retry_of FROM chat_messages WHERE id = $1`, acc.MessageID).Scan(&retryOf))
	if retryOf == nil || *retryOf != failed {
		t.Fatal("retry_of not set")
	}
	if items, _ := cfgSvc.Focus(ctx); len(items) != 0 {
		t.Fatalf("problem not cleared after a success: %+v", items)
	}

	// 5. PI-08: the idle session is saved and closed; the next message restores it.
	hub.reset()
	chat.IdleTimeout = time.Minute // saveAndClose of sessions idle for a minute minus a minute
	rctx, cancel := context.WithCancel(ctx)
	go chat.Run(rctx)
	time.Sleep(61 * time.Second) // the reaper ticks every minute
	cancel()
	var snapKey *string
	must(t, pool.QueryRow(ctx, `SELECT snapshot_key FROM pi_sessions WHERE user_id = $1 AND closed_at IS NULL`, admin.UserID).Scan(&snapKey))
	if snapKey == nil || len(objects.m[*snapKey]) == 0 {
		t.Fatal("the session file was not saved")
	}
	id = send("which word?")
	done(id)
	rs = llm.Requests()
	found := false
	for _, m := range rs[len(rs)-1].Messages {
		found = found || strings.Contains(m.Text(), "apricot")
	}
	if !found {
		t.Fatal("the restored session lost the conversation")
	}

	// USE-01: chat usage rows with scenario, connection, model and cost.
	var rows int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM agent_usage WHERE scenario = 'chat' AND connection_id = $1 AND cost_usd > 0 AND model IS NOT NULL`, c.ID).Scan(&rows))
	if rows < 3 {
		t.Fatalf("usage rows %d", rows)
	}
}
