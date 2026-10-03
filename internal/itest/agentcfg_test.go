//go:build integration

package itest

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agentcfg"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
)

type fakeOperator struct{}

func (fakeOperator) CheckLLM(_ context.Context, req agent.LLMCheckRequest) (agent.LLMCheckResponse, error) {
	var out agent.LLMCheckResponse
	for _, m := range req.Model.Models {
		if req.Secrets.LLMKey == "sk-broke-0000000000" {
			out.Results = append(out.Results, agent.LLMCheckResult{Model: m.ID, ErrorClass: agent.ErrInsufficientBalance, HTTPStatus: 402})
		} else {
			out.Results = append(out.Results, agent.LLMCheckResult{Model: m.ID, OK: true, LatencyMs: 120})
		}
	}
	return out, nil
}

func (fakeOperator) CheckMCP(context.Context, agent.MCPCheckRequest) (agent.MCPCheckResponse, error) {
	return agent.MCPCheckResponse{OK: true, Tools: []agent.MCPTool{{Name: "search", ReadOnly: true}}}, nil
}

type memStorage struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStorage) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, _ := io.ReadAll(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[key] = b
	return nil
}
func (s *memStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return io.NopCloser(bytes.NewReader(s.m[key])), nil
}
func (s *memStorage) Delete(_ context.Context, key string) error { return nil }

func agentSvc(t *testing.T) (*agentcfg.Service, *domain.Principal) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{`DELETE FROM agent_usage`, `DELETE FROM agent_scenario_models`, `DELETE FROM agent_default_model`,
		`DELETE FROM mcp_servers`, `DELETE FROM agent_config_changes`, `DELETE FROM llm_connections`} {
		_, err := pool.Exec(ctx, q)
		must(t, err)
	}
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	must(t, err)
	id := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO users (id, provider_uid, username, display_name, agent_name, agent_tone, is_global_admin)
		VALUES ($1, $2, $2, 'Admin', 'Hammurapi', 'business', true)`, id, "admin-"+id.String()[:8])
	must(t, err)
	// Other tests count global administrators: remove this one afterwards.
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return agentcfg.NewService(pool, box, fakeOperator{}, nil, &memStorage{}, nil, nil, "main"),
		&domain.Principal{UserID: id, GlobalAdmin: true}
}

func code(t *testing.T, err error, want string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Code != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
}

// CON-01, CON-06, CON-07, CON-08, CON-12.
func TestAgentConnections(t *testing.T) {
	ctx := context.Background()
	s, admin := agentSvc(t)
	_, err := s.Resolve(ctx, agent.ScenarioChat)
	code(t, err, "agent_not_configured")

	const key = "sk-live-secret-a1b2"
	c, err := s.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Type: agentcfg.TypeDeepSeek, APIKey: key})
	must(t, err)
	if c.Name != "DeepSeek" || c.BaseURL != "https://api.deepseek.com" || len(c.Models) != 2 || c.KeyLast4 != "a1b2" {
		t.Fatalf("%+v", c)
	}
	var leaks int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_connections WHERE name || base_url || models::text || api_key_last4 LIKE '%'||$1||'%'`, key).Scan(&leaks))
	must(t, pool.QueryRow(ctx, `SELECT count(*) + $2 FROM agent_config_changes WHERE summary LIKE '%'||$1||'%'`, key, leaks).Scan(&leaks))
	if leaks != 0 {
		t.Fatal("the key is stored in plain text")
	}
	if _, err := s.Resolve(ctx, agent.ScenarioChat); err == nil {
		t.Fatal("resolved without a default model")
	}
	_, err = s.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: c.ID, Model: "deepseek-v4-flash"}})
	must(t, err)
	r, err := s.Resolve(ctx, agent.ScenarioChat)
	must(t, err)
	if r.LLMKey != key || r.Model.ModelID != "deepseek-v4-flash" || r.Model.Thinking != "off" || r.Model.API != "openai-completions" {
		t.Fatalf("%+v", r)
	}
	_, err = s.ReplaceKey(ctx, admin, c.ID, "sk-new-key-zz99")
	must(t, err)
	r, _ = s.Resolve(ctx, agent.ScenarioChat)
	if r.LLMKey != "sk-new-key-zz99" {
		t.Fatal("the next session does not use the new key")
	}
	// CON-05: a draft check stores nothing.
	var before, after int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_connections`).Scan(&before))
	res, err := s.CheckDraft(ctx, admin, agentcfg.ConnectionInput{Type: agentcfg.TypeDeepSeek, APIKey: "sk-draft-1234567"})
	must(t, err)
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_connections`).Scan(&after))
	if len(res.Results) != 2 || before != after {
		t.Fatal("draft check")
	}
}

// MOD-01, MOD-02, MOD-03, CON-09, CON-10.
func TestAgentScenarioModels(t *testing.T) {
	ctx := context.Background()
	s, admin := agentSvc(t)
	a, err := s.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Name: "Main", Type: agentcfg.TypeDeepSeek, APIKey: "sk-aaaa-11111111"})
	must(t, err)
	b, err := s.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Name: "Code", Type: agentcfg.TypeDeepSeek, APIKey: "sk-bbbb-22222222"})
	must(t, err)
	_, err = s.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{
		Default:   &agentcfg.ModelChoice{ConnectionID: a.ID, Model: "deepseek-v4-flash"},
		Scenarios: map[agent.Scenario]*agentcfg.ModelChoice{agent.ScenarioCodegen: {ConnectionID: b.ID, Model: "deepseek-v4-pro", Thinking: "xhigh"}}})
	must(t, err)
	cg, err := s.Resolve(ctx, agent.ScenarioCodegen)
	must(t, err)
	an, err := s.Resolve(ctx, agent.ScenarioIssueAnalysis)
	must(t, err)
	if cg.Model.ModelID != "deepseek-v4-pro" || cg.Model.Thinking != "xhigh" || cg.LLMKey != "sk-bbbb-22222222" || an.Model.ModelID != "deepseek-v4-flash" {
		t.Fatalf("codegen %+v analysis %+v", cg.Model, an.Model)
	}
	_, err = s.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: a.ID, Model: "deepseek-v4-flash", Thinking: "medium"}})
	code(t, err, "thinking_not_supported")
	_, err = s.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: a.ID, Model: "gpt-9"}})
	code(t, err, "invalid_model")
	err = s.DeleteConnection(ctx, admin, b.ID)
	code(t, err, "connection_in_use")
	off := false
	_, err = s.UpdateConnection(ctx, admin, b.ID, agentcfg.ConnectionPatch{Enabled: &off})
	must(t, err)
	cg, err = s.Resolve(ctx, agent.ScenarioCodegen)
	must(t, err)
	if cg.Model.ModelID != "deepseek-v4-flash" || cg.ConnectionID != a.ID {
		t.Fatalf("disabled override not replaced by the default: %+v", cg.Model)
	}
	resolved, err := s.ResolvedModels(ctx, admin)
	must(t, err)
	if len(resolved) != len(agent.Scenarios) || !resolved[0].Inherited {
		t.Fatalf("%+v", resolved)
	}
}

// CON-11: three replicas bootstrap at once — one connection.
func TestAgentBootstrapOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := agentSvc(t)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); must(t, s.Bootstrap(ctx, "sk-bootstrap-1234", "")) }()
	}
	wg.Wait()
	var n, d int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_connections`).Scan(&n))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM agent_default_model WHERE model = 'deepseek-v4-flash'`).Scan(&d))
	if n != 1 || d != 1 {
		t.Fatalf("connections %d defaults %d", n, d)
	}
	must(t, s.Bootstrap(ctx, "sk-other-key-5678", ""))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_connections`).Scan(&n))
	if n != 1 {
		t.Fatal("bootstrap ran again")
	}
}

// MCP-03, MCP-01 (resolution part): header values never leave in the API; a
// server reaches only its scenarios.
func TestAgentMCP(t *testing.T) {
	ctx := context.Background()
	s, admin := agentSvc(t)
	c, err := s.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Type: agentcfg.TypeDeepSeek, APIKey: "sk-mcp-test-0000"})
	must(t, err)
	_, err = s.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: c.ID, Model: "deepseek-v4-flash"}})
	must(t, err)
	m, err := s.CreateMCP(ctx, admin, agentcfg.MCPInput{Name: "jira", URL: "https://mcp.example.com/jira",
		Headers: []agentcfg.HeaderInput{{Name: "Authorization", Value: "Bearer jira-token-9f9f"}}, Scenarios: []agent.Scenario{agent.ScenarioChat, agent.ScenarioIssueAnalysis}})
	must(t, err)
	if m.Exposure != "deferred" || len(m.Headers) != 1 || m.Headers[0].Last4 != "9f9f" {
		t.Fatalf("%+v", m)
	}
	list, err := s.MCPServers(ctx, admin)
	must(t, err)
	if !list[0].Builtin || list[0].Name != "hammurapi" || len(list) != 2 {
		t.Fatalf("%+v", list)
	}
	chat, err := s.Resolve(ctx, agent.ScenarioChat)
	must(t, err)
	if len(chat.MCP) != 1 || chat.MCPHeaders["jira"]["Authorization"] != "Bearer jira-token-9f9f" {
		t.Fatalf("%+v", chat.MCP)
	}
	cg, err := s.Resolve(ctx, agent.ScenarioCodegen)
	must(t, err)
	if len(cg.MCP) != 0 {
		t.Fatal("jira reached code generation")
	}
	// Changing without a header value keeps the stored value.
	_, err = s.UpdateMCP(ctx, admin, *m.ID, agentcfg.MCPInput{Headers: []agentcfg.HeaderInput{{Name: "Authorization"}}})
	must(t, err)
	chat, _ = s.Resolve(ctx, agent.ScenarioChat)
	if chat.MCPHeaders["jira"]["Authorization"] != "Bearer jira-token-9f9f" || chat.MCP[0].Exposure != "deferred" {
		t.Fatal("header value or exposure lost on update")
	}
}

// ERR-09: 402 marks the connection and adds a focus item; a success clears both.
func TestAgentConnectionStatus(t *testing.T) {
	ctx := context.Background()
	s, admin := agentSvc(t)
	c, err := s.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Type: agentcfg.TypeDeepSeek, APIKey: "sk-broke-0000000000"})
	must(t, err)
	_, err = s.PutScenarioModels(ctx, admin, agentcfg.ScenarioModels{Default: &agentcfg.ModelChoice{ConnectionID: c.ID, Model: "deepseek-v4-flash"}})
	must(t, err)
	if _, err := s.CheckConnection(ctx, admin, c.ID); err != nil {
		t.Fatal(err)
	}
	items, err := s.Focus(ctx)
	must(t, err)
	if len(items) != 1 || items[0].Action != "connection_problem" || *items[0].Hint != "insufficient_balance" {
		t.Fatalf("%+v", items)
	}
	_, err = pool.Exec(ctx, `UPDATE llm_connections SET status_at = now() - interval '2 minutes'`)
	must(t, err)
	s.RecordResult(ctx, c.ID, "")
	items, err = s.Focus(ctx)
	must(t, err)
	if len(items) != 0 {
		t.Fatalf("problem not cleared: %+v", items)
	}
}

// USE-04: grouped sums equal the totals.
func TestAgentUsageReport(t *testing.T) {
	ctx := context.Background()
	s, admin := agentSvc(t)
	c, err := s.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Type: agentcfg.TypeDeepSeek, APIKey: "sk-usage-00000000"})
	must(t, err)
	for i, sc := range []agent.Scenario{agent.ScenarioChat, agent.ScenarioChat, agent.ScenarioIssueAnalysis, agent.ScenarioCodegen} {
		model := "deepseek-v4-flash"
		if sc == agent.ScenarioCodegen {
			model = "deepseek-v4-pro"
		}
		must(t, s.RecordUsage(ctx, agentcfg.UsageRecord{Scenario: sc, ConnectionID: &c.ID, Model: model,
			Usage: agent.Usage{TokensIn: int64(1000 * (i + 1)), TokensOut: 100, CacheRead: 500, CostUSD: 0.01 * float64(i+1)}}))
	}
	rep, err := s.Usage(ctx, admin, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{"connection", "model"})
	must(t, err)
	var cost float64
	var in int64
	for _, r := range rep.Rows {
		cost += r.CostUSD
		in += r.TokensIn
	}
	if rep.Totals.Runs != 4 || in != rep.Totals.TokensIn || abs(cost-rep.Totals.CostUSD) > 1e-9 || len(rep.Rows) != 2 {
		t.Fatalf("%+v", rep)
	}
	byScenario, err := s.Usage(ctx, admin, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{"scenario"})
	must(t, err)
	if byScenario.Rows[0].Key["scenario"] == "" {
		t.Fatalf("%+v", byScenario.Rows)
	}
	_, err = s.Usage(ctx, admin, time.Now(), time.Now().Add(-time.Hour), nil)
	code(t, err, "invalid_period")
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
