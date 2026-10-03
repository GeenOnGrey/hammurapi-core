package agentcfg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// SessionConfig is everything a Pi session of a scenario needs, resolved
// from the Agent section at the moment the session opens (R10).
type SessionConfig struct {
	Scenario       agent.Scenario
	ConnectionID   uuid.UUID
	ConnectionName string
	Model          agent.ModelSpec
	LLMKey         string
	MCP            []agent.MCPServer
	MCPHeaders     map[string]map[string]string
	Skills         *agent.Skills
}

// Request builds the operator's session request; the caller adds its MCP
// URL and token, the system instructions and the snapshot or history.
func (c *SessionConfig) Request(kind agent.SessionKind) agent.SessionRequest {
	return agent.SessionRequest{Scenario: c.Scenario, Kind: kind, Model: c.Model,
		Secrets: agent.Secrets{LLMKey: c.LLMKey, MCPHeaders: c.MCPHeaders}, MCP: c.MCP, Skills: c.Skills}
}

// Resolve returns the session configuration of a scenario or the 409
// agent_not_configured error (CON-12).
func (s *Service) Resolve(ctx context.Context, sc agent.Scenario) (*SessionConfig, error) {
	rm, err := s.resolveModel(ctx, sc)
	if err != nil {
		return nil, err
	}
	key, err := s.key(ctx, rm.conn.ID)
	if err != nil {
		return nil, err
	}
	cfg := &SessionConfig{Scenario: sc, ConnectionID: rm.conn.ID, ConnectionName: rm.conn.Name, LLMKey: key,
		Model: agent.ModelSpec{Provider: "hmr-" + shortID(rm.conn.ID.String()), ConnectionID: rm.conn.ID.String(),
			API: Presets[rm.conn.Type].API, BaseURL: rm.conn.BaseURL, ModelID: rm.model.ID, Thinking: rm.thinking, Models: rm.conn.Models},
		MCPHeaders: map[string]map[string]string{}}
	rows, err := s.pool.Query(ctx, `SELECT id, name, url, exposure FROM mcp_servers WHERE enabled AND $1 = ANY(scenarios::text[]) ORDER BY name`, string(sc))
	if err != nil {
		return nil, err
	}
	type srv struct {
		id                  uuid.UUID
		name, url, exposure string
	}
	var servers []srv
	for rows.Next() {
		var x srv
		if err := rows.Scan(&x.id, &x.name, &x.url, &x.exposure); err != nil {
			rows.Close()
			return nil, err
		}
		servers = append(servers, x)
	}
	rows.Close()
	for _, x := range servers {
		vals, err := s.headerValues(ctx, s.pool, x.id)
		if err != nil {
			return nil, fmt.Errorf("MCP server %s: %w", x.name, err)
		}
		names := make([]string, 0, len(vals))
		for n := range vals {
			names = append(names, n)
		}
		cfg.MCP = append(cfg.MCP, agent.MCPServer{Name: x.name, URL: x.url, HeaderNames: names, Exposure: x.exposure})
		cfg.MCPHeaders[x.name] = vals
	}
	if cfg.Skills, err = s.skillsFor(ctx, sc); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ─── usage (R19) ────────────────────────────────────────────────────

// UsageRecord is one row of agent_usage.
type UsageRecord struct {
	Scenario     agent.Scenario
	Context      string // free-form context: chat, discovery, gategen, check, task
	ConnectionID *uuid.UUID
	Model        string
	UserID       *uuid.UUID
	IssueID      *uuid.UUID
	FeatureID    *uuid.UUID
	ReleaseID    *uuid.UUID
	TaskID       *uuid.UUID
	agent.Usage
}

// RecordUsage stores usage; zero usage is skipped.
func (s *Service) RecordUsage(ctx context.Context, r UsageRecord) error {
	if r.IsZero() {
		return nil
	}
	if r.Context == "" {
		r.Context = string(r.Scenario)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO agent_usage (context, scenario, connection_id, model, user_id, issue_id, feature_id, release_id, task_id,
		tokens_in, tokens_out, cache_read, cache_write, cost_usd) VALUES ($1,$2::agent_scenario,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		r.Context, string(r.Scenario), r.ConnectionID, r.Model, r.UserID, r.IssueID, r.FeatureID, r.ReleaseID, r.TaskID,
		r.TokensIn, r.TokensOut, r.CacheRead, r.CacheWrite, r.CostUSD)
	return err
}

// UsageTotals are the four indicators of the Usage page.
type UsageTotals struct {
	CostUSD    float64 `json:"costUsd"`
	TokensIn   int64   `json:"tokensIn"`
	TokensOut  int64   `json:"tokensOut"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Runs       int64   `json:"runs"`
}

// UsageRow is a group of the report.
type UsageRow struct {
	Key       map[string]string `json:"key"`
	CostUSD   float64           `json:"costUsd"`
	TokensIn  int64             `json:"tokensIn"`
	TokensOut int64             `json:"tokensOut"`
	CacheRead int64             `json:"cacheRead"`
	Runs      int64             `json:"runs"`
}

// UsageReport is GET /usage.
type UsageReport struct {
	From   time.Time   `json:"from"`
	To     time.Time   `json:"to"`
	Totals UsageTotals `json:"totals"`
	Rows   []UsageRow  `json:"rows"`
}

var groupColumns = map[string]string{
	"scenario":   "COALESCE(u.scenario::text, u.context)",
	"connection": "COALESCE(c.name, '')",
	"model":      "COALESCE(u.model, '')",
	"day":        "to_char(date_trunc('day', u.created_at), 'YYYY-MM-DD')",
}

// Usage aggregates usage over [from, to) grouped by any of scenario,
// connection, model, day (USE-04).
func (s *Service) Usage(ctx context.Context, p *domain.Principal, from, to time.Time, groupBy []string) (*UsageReport, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, apperr.Unprocessable("invalid_period", "to must be after from")
	}
	var cols, keys []string
	for _, g := range groupBy {
		col, ok := groupColumns[g]
		if !ok {
			return nil, apperr.Unprocessable("invalid_group", "groupBy is scenario, connection, model or day")
		}
		cols, keys = append(cols, col), append(keys, g)
	}
	rep := &UsageReport{From: from, To: to, Rows: []UsageRow{}}
	const sums = `COALESCE(sum(u.cost_usd),0)::float8, COALESCE(sum(u.tokens_in),0), COALESCE(sum(u.tokens_out),0),
		COALESCE(sum(u.cache_read),0), COALESCE(sum(u.cache_write),0), count(*)`
	const from_ = ` FROM agent_usage u LEFT JOIN llm_connections c ON c.id = u.connection_id WHERE u.created_at >= $1 AND u.created_at < $2`
	if err := s.pool.QueryRow(ctx, `SELECT `+sums+from_, from, to).Scan(&rep.Totals.CostUSD, &rep.Totals.TokensIn, &rep.Totals.TokensOut,
		&rep.Totals.CacheRead, &rep.Totals.CacheWrite, &rep.Totals.Runs); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return rep, nil
	}
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = c + " AS g" + fmt.Sprint(i)
	}
	grp := make([]string, len(cols))
	for i := range cols {
		grp[i] = fmt.Sprint(i + 1)
	}
	q := `SELECT ` + strings.Join(sel, ", ") + `, ` + sums + from_ + ` GROUP BY ` + strings.Join(grp, ", ") +
		` ORDER BY ` + fmt.Sprint(len(cols)+1) + ` DESC`
	rows, err := s.pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]string, len(cols))
		dest := make([]any, 0, len(cols)+6)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var r UsageRow
		var cacheWrite int64
		dest = append(dest, &r.CostUSD, &r.TokensIn, &r.TokensOut, &r.CacheRead, &cacheWrite, &r.Runs)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.Key = map[string]string{}
		for i, k := range keys {
			r.Key[k] = vals[i]
		}
		rep.Rows = append(rep.Rows, r)
	}
	return rep, rows.Err()
}

// ─── audit log ──────────────────────────────────────────────────────

// AuditEntry is a change of the Agent section.
type AuditEntry struct {
	ID           uuid.UUID `json:"id"`
	ObjectType   string    `json:"objectType"`
	ObjectRef    string    `json:"objectRef"`
	Action       string    `json:"action"`
	Summary      string    `json:"summary"`
	Actor        *string   `json:"actor"`
	PRURL        *string   `json:"prUrl"`
	SelfApproved bool      `json:"selfApproved"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Audit lists the audit log, newest first.
func (s *Service) Audit(ctx context.Context, p *domain.Principal, page httpx.Page) (httpx.List[AuditEntry], error) {
	if err := requireGlobal(p); err != nil {
		return httpx.List[AuditEntry]{}, err
	}
	args := []any{page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` WHERE (a.created_at, a.id::text) < ($2, $3)`
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id, a.object_type, a.object_ref, a.action, a.summary, u.display_name, a.pr_url, a.self_approved, a.created_at
		FROM agent_config_changes a LEFT JOIN users u ON u.id = a.actor_id`+cond+` ORDER BY a.created_at DESC, a.id::text DESC LIMIT $1`, args...)
	if err != nil {
		return httpx.List[AuditEntry]{}, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.ObjectType, &a.ObjectRef, &a.Action, &a.Summary, &a.Actor, &a.PRURL, &a.SelfApproved, &a.CreatedAt); err != nil {
			return httpx.List[AuditEntry]{}, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return httpx.List[AuditEntry]{}, err
	}
	return httpx.NewList(out, page.Limit, func(a AuditEntry) (time.Time, string) { return a.CreatedAt, a.ID.String() }), nil
}

// ─── focus (R21) ────────────────────────────────────────────────────

// FocusItem is an Agent element of "In focus" for global administrators.
type FocusItem struct {
	Kind         string    `json:"kind"` // connection | mcp_server | agent
	Key          string    `json:"key"`
	Title        string    `json:"title"`
	Action       string    `json:"action"` // agent_not_configured | connection_problem | mcp_problem
	WaitingSince time.Time `json:"waitingSince"`
	Hint         *string   `json:"hint,omitempty"`
}

// Focus returns the Agent group of "In focus".
func (s *Service) Focus(ctx context.Context) ([]FocusItem, error) {
	out := []FocusItem{}
	var n int
	var since time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(min(created_at), now()) FROM llm_connections WHERE enabled`).Scan(&n, &since); err != nil {
		return nil, err
	}
	var hasDefault bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_default_model)`).Scan(&hasDefault); err != nil {
		return nil, err
	}
	if n == 0 || !hasDefault {
		out = append(out, FocusItem{Kind: "agent", Key: "setup", Title: "LLM", Action: "agent_not_configured", WaitingSince: since})
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, name, status::text, COALESCE(status_at, updated_at) FROM llm_connections
		WHERE enabled AND status IN ('insufficient_balance','auth')`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it FocusItem
		var status string
		if err := rows.Scan(&it.Key, &it.Title, &status, &it.WaitingSince); err != nil {
			rows.Close()
			return nil, err
		}
		it.Kind, it.Action, it.Hint = "connection", "connection_problem", &status
		out = append(out, it)
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT id::text, name, COALESCE(status_reason,''), COALESCE(status_at, updated_at) FROM mcp_servers
		WHERE enabled AND status = 'error'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it FocusItem
		var reason string
		if err := rows.Scan(&it.Key, &it.Title, &reason, &it.WaitingSince); err != nil {
			return nil, err
		}
		it.Kind, it.Action, it.Hint = "mcp_server", "mcp_problem", &reason
		out = append(out, it)
	}
	return out, rows.Err()
}

// ScenariosOn lists the scenarios that resolve to a connection (for the
// problem card of the connections page).
func (s *Service) ScenariosOn(ctx context.Context, p *domain.Principal) (map[string][]agent.Scenario, error) {
	res, err := s.ResolvedModels(ctx, p)
	if err != nil {
		return nil, err
	}
	out := map[string][]agent.Scenario{}
	for _, r := range res {
		out[r.ConnectionID.String()] = append(out[r.ConnectionID.String()], r.Scenario)
	}
	return out, nil
}
