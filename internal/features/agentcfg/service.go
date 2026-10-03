package agentcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/storage"
)

// Event types of the Agent section (tech spec §3).
const (
	EventConnectionStatus = "agent.connection_status"
)

// Operator is the part of the operator's client the section needs.
type Operator interface {
	CheckLLM(ctx context.Context, req agent.LLMCheckRequest) (agent.LLMCheckResponse, error)
	CheckMCP(ctx context.Context, req agent.MCPCheckRequest) (agent.MCPCheckResponse, error)
}

// Service implements the Agent section.
type Service struct {
	pool     *pgxpool.Pool
	box      *crypto.Box
	operator Operator
	hub      events.Publisher
	store    storage.Storage

	// Skills: changes through PRs in the specification repository.
	git           git.Provider
	tokens        git.TokenSource
	defaultBranch string
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, box *crypto.Box, op Operator, hub events.Publisher, store storage.Storage,
	provider git.Provider, tokens git.TokenSource, defaultBranch string) *Service {
	return &Service{pool: pool, box: box, operator: op, hub: hub, store: store, git: provider, tokens: tokens, defaultBranch: defaultBranch}
}

// requireGlobal: the whole section is for global administrators only (R5).
func requireGlobal(p *domain.Principal) error {
	if p == nil || !p.GlobalAdmin {
		return apperr.Forbidden("forbidden", "the Agent section is available to global administrators only")
	}
	return nil
}

// ─── connections ────────────────────────────────────────────────────

// Connection is an LLM connection as the API shows it (never the key).
type Connection struct {
	ID           uuid.UUID        `json:"id"`
	Name         string           `json:"name"`
	Type         ConnectionType   `json:"type"`
	BaseURL      string           `json:"baseUrl"`
	Models       []agent.ModelDef `json:"models"`
	KeyLast4     string           `json:"keyLast4"`
	Enabled      bool             `json:"enabled"`
	Status       string           `json:"status"`
	StatusReason *string          `json:"statusReason"`
	StatusAt     *time.Time       `json:"statusAt"`
	CreatedAt    time.Time        `json:"createdAt"`
}

const connSelect = `SELECT id, name, type, base_url, models, api_key_last4, enabled, status, status_reason, status_at, created_at FROM llm_connections`

func scanConn(row pgx.Row) (*Connection, error) {
	var c Connection
	var models []byte
	if err := row.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &models, &c.KeyLast4, &c.Enabled, &c.Status, &c.StatusReason, &c.StatusAt, &c.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(models, &c.Models); err != nil {
		return nil, err
	}
	if !c.Enabled {
		c.Status = "disabled"
	}
	return &c, nil
}

// Connections lists the connections.
func (s *Service) Connections(ctx context.Context, p *domain.Principal) ([]Connection, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	return s.connections(ctx)
}

func (s *Service) connections(ctx context.Context) ([]Connection, error) {
	rows, err := s.pool.Query(ctx, connSelect+` ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Service) connection(ctx context.Context, id uuid.UUID) (*Connection, error) {
	c, err := scanConn(s.pool.QueryRow(ctx, connSelect+` WHERE id = $1`, id))
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("connection_not_found", "LLM connection not found")
	}
	return c, err
}

// ConnectionInput is the body of POST /connections and POST /connections/check.
type ConnectionInput struct {
	Name   string         `json:"name"`
	Type   ConnectionType `json:"type"`
	APIKey string         `json:"apiKey"`
	Models []string       `json:"models"`
}

func (in *ConnectionInput) validate() (Preset, []agent.ModelDef, error) {
	pr, ok := Presets[in.Type]
	if !ok {
		return Preset{}, nil, apperr.Unprocessable("invalid_type", "unknown connection type").With("field", "type")
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = pr.Name
	}
	if len(in.Name) > 80 {
		return pr, nil, apperr.Unprocessable("invalid_name", "the name is too long").With("field", "name")
	}
	in.APIKey = strings.TrimSpace(in.APIKey)
	if len(in.APIKey) < 8 {
		return pr, nil, apperr.Unprocessable("invalid_key", "the API key is required").With("field", "apiKey")
	}
	models, ok := presetModels(pr, in.Models)
	if !ok || len(models) == 0 {
		return pr, nil, apperr.Unprocessable("invalid_models", "the models must be from the type's list").With("field", "models")
	}
	return pr, models, nil
}

func last4(key string) string {
	if len(key) <= 4 {
		return key
	}
	return key[len(key)-4:]
}

// CreateConnection stores a connection with the encrypted key (R5, R7).
func (s *Service) CreateConnection(ctx context.Context, p *domain.Principal, in ConnectionInput) (*Connection, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	pr, models, err := in.validate()
	if err != nil {
		return nil, err
	}
	return s.createConnection(ctx, &p.UserID, in.Name, pr, models, in.APIKey)
}

func (s *Service) createConnection(ctx context.Context, actor *uuid.UUID, name string, pr Preset, models []agent.ModelDef, key string) (*Connection, error) {
	enc, err := s.box.Seal(key)
	if err != nil {
		return nil, err
	}
	mj, _ := json.Marshal(models)
	id := uuid.New()
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO llm_connections (id, name, type, base_url, models, api_key_enc, api_key_last4, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, name, pr.Type, pr.BaseURL, mj, enc, last4(key), actor); err != nil {
			return err
		}
		return audit(ctx, tx, actor, "connection", id.String(), "create", fmt.Sprintf("connection %q (%s, models %s)", name, pr.Type, modelIDs(models)))
	})
	if postgres.IsUniqueViolation(err) {
		return nil, apperr.Conflict("connection_name_taken", "a connection with this name already exists").With("field", "name")
	}
	if err != nil {
		return nil, err
	}
	return s.connection(ctx, id)
}

func modelIDs(ms []agent.ModelDef) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return strings.Join(ids, ", ")
}

// ConnectionPatch is the body of PATCH /connections/{id}.
type ConnectionPatch struct {
	Name    *string  `json:"name"`
	Models  []string `json:"models"`
	Enabled *bool    `json:"enabled"`
}

// UpdateConnection renames, changes the models or enables/disables (R8).
func (s *Service) UpdateConnection(ctx context.Context, p *domain.Principal, id uuid.UUID, in ConnectionPatch) (*Connection, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	c, err := s.connection(ctx, id)
	if err != nil {
		return nil, err
	}
	var changes []string
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if in.Name != nil {
			n := strings.TrimSpace(*in.Name)
			if n == "" || len(n) > 80 {
				return apperr.Unprocessable("invalid_name", "the name is required").With("field", "name")
			}
			if _, err := tx.Exec(ctx, `UPDATE llm_connections SET name = $2, updated_at = now() WHERE id = $1`, id, n); err != nil {
				if postgres.IsUniqueViolation(err) {
					return apperr.Conflict("connection_name_taken", "a connection with this name already exists").With("field", "name")
				}
				return err
			}
			changes = append(changes, fmt.Sprintf("name %q", n))
		}
		if in.Models != nil {
			models, ok := presetModels(Presets[c.Type], in.Models)
			if !ok || len(models) == 0 {
				return apperr.Unprocessable("invalid_models", "the models must be from the type's list").With("field", "models")
			}
			if used, err := s.modelsInUse(ctx, tx, id, models); err != nil {
				return err
			} else if len(used) > 0 {
				return apperr.Conflict("connection_in_use", "removed models are assigned to scenarios").With("scenarios", used)
			}
			mj, _ := json.Marshal(models)
			if _, err := tx.Exec(ctx, `UPDATE llm_connections SET models = $2, updated_at = now() WHERE id = $1`, id, mj); err != nil {
				return err
			}
			changes = append(changes, "models "+modelIDs(models))
		}
		if in.Enabled != nil {
			if _, err := tx.Exec(ctx, `UPDATE llm_connections SET enabled = $2, updated_at = now() WHERE id = $1`, id, *in.Enabled); err != nil {
				return err
			}
			if *in.Enabled {
				changes = append(changes, "enabled")
			} else {
				changes = append(changes, "disabled")
			}
		}
		if len(changes) == 0 {
			return nil
		}
		return audit(ctx, tx, &p.UserID, "connection", id.String(), "update", fmt.Sprintf("connection %q: %s", c.Name, strings.Join(changes, ", ")))
	})
	if err != nil {
		return nil, err
	}
	if len(changes) > 0 {
		s.publishFocus(ctx)
	}
	return s.connection(ctx, id)
}

// modelsInUse lists scenarios (or "default") whose model would disappear.
func (s *Service) modelsInUse(ctx context.Context, q postgres.Querier, id uuid.UUID, keep []agent.ModelDef) ([]string, error) {
	kept := map[string]bool{}
	for _, m := range keep {
		kept[m.ID] = true
	}
	rows, err := q.Query(ctx, `SELECT scenario::text, model FROM agent_scenario_models WHERE connection_id = $1
		UNION ALL SELECT 'default', model FROM agent_default_model WHERE connection_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var used []string
	for rows.Next() {
		var sc, m string
		if err := rows.Scan(&sc, &m); err != nil {
			return nil, err
		}
		if !kept[m] {
			used = append(used, sc)
		}
	}
	return used, rows.Err()
}

// ReplaceKey stores a new key; the next sessions use it (R7, CON-08).
func (s *Service) ReplaceKey(ctx context.Context, p *domain.Principal, id uuid.UUID, key string) (*Connection, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if len(key) < 8 {
		return nil, apperr.Unprocessable("invalid_key", "the API key is required").With("field", "apiKey")
	}
	c, err := s.connection(ctx, id)
	if err != nil {
		return nil, err
	}
	enc, err := s.box.Seal(key)
	if err != nil {
		return nil, err
	}
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE llm_connections SET api_key_enc = $2, api_key_last4 = $3, status = 'unknown',
			status_reason = NULL, status_at = NULL, updated_at = now() WHERE id = $1`, id, enc, last4(key)); err != nil {
			return err
		}
		return audit(ctx, tx, &p.UserID, "connection", id.String(), "replace_key", fmt.Sprintf("connection %q: key replaced (…%s)", c.Name, last4(key)))
	})
	if err != nil {
		return nil, err
	}
	s.publishFocus(ctx)
	return s.connection(ctx, id)
}

// DeleteConnection deletes an unused connection (R8, CON-09).
func (s *Service) DeleteConnection(ctx context.Context, p *domain.Principal, id uuid.UUID) error {
	if err := requireGlobal(p); err != nil {
		return err
	}
	c, err := s.connection(ctx, id)
	if err != nil {
		return err
	}
	used, err := s.modelsInUse(ctx, s.pool, id, nil)
	if err != nil {
		return err
	}
	if len(used) > 0 {
		return apperr.Conflict("connection_in_use", "the connection is used; move these scenarios to another connection first").With("scenarios", used)
	}
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM llm_connections WHERE id = $1`, id); err != nil {
			if postgres.IsForeignKeyViolation(err) {
				return apperr.Conflict("connection_in_use", "the connection is used by scenarios")
			}
			return err
		}
		return audit(ctx, tx, &p.UserID, "connection", id.String(), "delete", fmt.Sprintf("connection %q deleted", c.Name))
	})
	if err == nil {
		s.publishFocus(ctx)
	}
	return err
}

// CheckConnection probes a saved connection and updates its status (R6, R21).
func (s *Service) CheckConnection(ctx context.Context, p *domain.Principal, id uuid.UUID) (*agent.LLMCheckResponse, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	c, err := s.connection(ctx, id)
	if err != nil {
		return nil, err
	}
	key, err := s.key(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := s.check(ctx, c.ID.String(), c.BaseURL, c.Models, key)
	if err != nil {
		return nil, err
	}
	// A check is a request like any other: it updates the connection status.
	for _, r := range res.Results {
		if r.OK {
			s.RecordResult(ctx, c.ID, "")
			return res, nil
		}
	}
	if len(res.Results) > 0 {
		s.RecordResult(ctx, c.ID, res.Results[0].ErrorClass)
	}
	return res, nil
}

// CheckDraft probes a connection that is not saved yet (CON-05): nothing is stored.
func (s *Service) CheckDraft(ctx context.Context, p *domain.Principal, in ConnectionInput) (*agent.LLMCheckResponse, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	pr, models, err := in.validate()
	if err != nil {
		return nil, err
	}
	return s.check(ctx, "draft", pr.BaseURL, models, in.APIKey)
}

func (s *Service) check(ctx context.Context, id, baseURL string, models []agent.ModelDef, key string) (*agent.LLMCheckResponse, error) {
	if s.operator == nil {
		return nil, apperr.Unavailable("agent_unavailable", "the agent operator is not configured")
	}
	res, err := s.operator.CheckLLM(ctx, agent.LLMCheckRequest{Model: agent.ModelSpec{Provider: "hmr-" + shortID(id), ConnectionID: id,
		API: "openai-completions", BaseURL: baseURL, Models: models}, Secrets: agent.Secrets{LLMKey: key}})
	if err != nil {
		return nil, operatorError(err)
	}
	return &res, nil
}

func operatorError(err error) error {
	var be *agent.BusyError
	if errors.As(err, &be) {
		return apperr.Unavailable("agent_busy", "the agent is busy, retry later").With("retryAfter", int(be.RetryAfter.Seconds()))
	}
	slog.Error("agent operator call failed", "err", err)
	return apperr.Unavailable("agent_unavailable", "the agent operator is unavailable")
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// key decrypts the API key of a connection.
func (s *Service) key(ctx context.Context, id uuid.UUID) (string, error) {
	var enc []byte
	if err := s.pool.QueryRow(ctx, `SELECT api_key_enc FROM llm_connections WHERE id = $1`, id).Scan(&enc); err != nil {
		if postgres.IsNoRows(err) {
			return "", apperr.NotFound("connection_not_found", "LLM connection not found")
		}
		return "", err
	}
	return s.box.Open(enc)
}

// RecordResult updates the connection status after a request (R21): success
// clears a problem; balance and auth errors mark it. Other classes are
// transient and do not change the status.
func (s *Service) RecordResult(ctx context.Context, id uuid.UUID, class agent.ErrorClass) {
	status, reason := "ok", ""
	switch class {
	case "":
	case agent.ErrInsufficientBalance:
		status, reason = "insufficient_balance", "402"
	case agent.ErrAuth:
		status, reason = "auth", "401"
	case agent.ErrUnavailable, agent.ErrRateLimit:
		status, reason = "unavailable", string(class)
	default:
		return
	}
	tag, err := s.pool.Exec(ctx, `UPDATE llm_connections SET status = $2, status_reason = NULLIF($3, ''), status_at = now()
		WHERE id = $1 AND (status <> $2 OR status_at IS NULL OR status_at < now() - interval '1 minute')`, id, status, reason)
	if err != nil {
		slog.WarnContext(ctx, "update connection status", "err", err)
		return
	}
	if tag.RowsAffected() > 0 && s.hub != nil {
		s.hub.Publish(ctx, events.Event{Type: EventConnectionStatus, Data: map[string]string{"connectionId": id.String(), "status": status}})
		s.publishFocus(ctx)
	}
}

func (s *Service) publishFocus(ctx context.Context) {
	if s.hub != nil {
		s.hub.Publish(ctx, events.Event{Type: events.FocusChanged, Data: map[string]string{"group": "agent"}})
	}
}

// ─── scenario models (R9, R10) ──────────────────────────────────────

// ModelChoice is a connection, a model and a level.
type ModelChoice struct {
	ConnectionID uuid.UUID `json:"connectionId"`
	Model        string    `json:"model"`
	Thinking     string    `json:"thinking"`
}

// ScenarioModels is GET/PUT /scenario-models.
type ScenarioModels struct {
	Default   *ModelChoice                    `json:"default"`
	Scenarios map[agent.Scenario]*ModelChoice `json:"scenarios"`
}

// GetScenarioModels returns the default and the overrides.
func (s *Service) GetScenarioModels(ctx context.Context, p *domain.Principal) (*ScenarioModels, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	return s.scenarioModels(ctx, s.pool)
}

func (s *Service) scenarioModels(ctx context.Context, q postgres.Querier) (*ScenarioModels, error) {
	out := &ScenarioModels{Scenarios: map[agent.Scenario]*ModelChoice{}}
	for _, sc := range agent.Scenarios {
		out.Scenarios[sc] = nil
	}
	var d ModelChoice
	err := q.QueryRow(ctx, `SELECT connection_id, model, thinking FROM agent_default_model`).Scan(&d.ConnectionID, &d.Model, &d.Thinking)
	if err == nil {
		out.Default = &d
	} else if !postgres.IsNoRows(err) {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT scenario::text, connection_id, model, thinking FROM agent_scenario_models`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc string
		var m ModelChoice
		if err := rows.Scan(&sc, &m.ConnectionID, &m.Model, &m.Thinking); err != nil {
			return nil, err
		}
		out.Scenarios[agent.Scenario(sc)] = &m
	}
	return out, rows.Err()
}

// validChoice checks connection, model and level (MOD-02, MOD-03).
func (s *Service) validChoice(ctx context.Context, field string, m *ModelChoice) error {
	c, err := s.connection(ctx, m.ConnectionID)
	if err != nil {
		return apperr.Unprocessable("invalid_connection", "unknown connection").With("field", field+".connectionId")
	}
	if !c.Enabled {
		return apperr.Unprocessable("connection_disabled", "the connection is disabled").With("field", field+".connectionId")
	}
	if m.Thinking == "" {
		m.Thinking = "off"
	}
	for _, md := range c.Models {
		if md.ID == m.Model {
			if !md.SupportsThinking(m.Thinking) {
				return apperr.Unprocessable("thinking_not_supported", "the model does not support this reasoning level").
					With("field", field+".thinking").With("supported", md.ThinkingLevels())
			}
			return nil
		}
	}
	return apperr.Unprocessable("invalid_model", "the connection has no such model").With("field", field+".model")
}

// PutScenarioModels replaces the default and the overrides; changes apply
// to the next sessions without a redeploy (R10).
func (s *Service) PutScenarioModels(ctx context.Context, p *domain.Principal, in ScenarioModels) (*ScenarioModels, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if in.Default == nil {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM llm_connections`).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, apperr.Unprocessable("default_required", "the default model is required").With("field", "default")
		}
	} else if err := s.validChoice(ctx, "default", in.Default); err != nil {
		return nil, err
	}
	for sc, m := range in.Scenarios {
		if !sc.Valid() {
			return nil, apperr.Unprocessable("invalid_scenario", "unknown scenario "+string(sc)).With("field", "scenarios")
		}
		if m != nil {
			if err := s.validChoice(ctx, "scenarios."+string(sc), m); err != nil {
				return nil, err
			}
		}
	}
	before, err := s.scenarioModels(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if in.Default != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO agent_default_model (id, connection_id, model, thinking, updated_by) VALUES (true,$1,$2,$3,$4)
				ON CONFLICT (id) DO UPDATE SET connection_id = $1, model = $2, thinking = $3, updated_by = $4, updated_at = now()`,
				in.Default.ConnectionID, in.Default.Model, in.Default.Thinking, p.UserID); err != nil {
				return err
			}
		}
		for _, sc := range agent.Scenarios {
			m, given := in.Scenarios[sc]
			if !given {
				continue // not in the body: unchanged
			}
			if m == nil {
				if _, err := tx.Exec(ctx, `DELETE FROM agent_scenario_models WHERE scenario = $1`, sc); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO agent_scenario_models (scenario, connection_id, model, thinking, updated_by) VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (scenario) DO UPDATE SET connection_id = $2, model = $3, thinking = $4, updated_by = $5, updated_at = now()`,
				sc, m.ConnectionID, m.Model, m.Thinking, p.UserID); err != nil {
				return err
			}
		}
		after, err := s.scenarioModels(ctx, tx)
		if err != nil {
			return err
		}
		return audit(ctx, tx, &p.UserID, "scenario_models", "models", "update", "models: "+diffModels(before, after))
	})
	if err != nil {
		return nil, err
	}
	s.publishFocus(ctx)
	return s.scenarioModels(ctx, s.pool)
}

func choiceText(m *ModelChoice) string {
	if m == nil {
		return "default"
	}
	return m.Model + "/" + m.Thinking
}

func diffModels(a, b *ScenarioModels) string {
	var parts []string
	if choiceText(a.Default) != choiceText(b.Default) {
		parts = append(parts, "default "+choiceText(a.Default)+" → "+choiceText(b.Default))
	}
	for _, sc := range agent.Scenarios {
		if choiceText(a.Scenarios[sc]) != choiceText(b.Scenarios[sc]) {
			parts = append(parts, string(sc)+" "+choiceText(a.Scenarios[sc])+" → "+choiceText(b.Scenarios[sc]))
		}
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, "; ")
}

// ResolvedModel is the effective model of a scenario (GET /scenario-models/resolved).
type ResolvedModel struct {
	Scenario       agent.Scenario `json:"scenario"`
	Stage          string         `json:"stage"`
	Inherited      bool           `json:"inherited"`
	ConnectionID   uuid.UUID      `json:"connectionId"`
	ConnectionName string         `json:"connectionName"`
	Model          string         `json:"model"`
	Thinking       string         `json:"thinking"`
	Status         string         `json:"status"`
}

// ResolvedModels lists the effective model of every scenario.
func (s *Service) ResolvedModels(ctx context.Context, p *domain.Principal) ([]ResolvedModel, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	out := []ResolvedModel{}
	for _, sc := range agent.Scenarios {
		r, err := s.resolveModel(ctx, sc)
		if err != nil {
			if ae, ok := apperr.As(err); ok && ae.Code == "agent_not_configured" {
				continue // this scenario has no model; later overrides may still resolve
			}
			return nil, err
		}
		out = append(out, ResolvedModel{Scenario: sc, Stage: StageOf[sc], Inherited: r.inherited, ConnectionID: r.conn.ID,
			ConnectionName: r.conn.Name, Model: r.model.ID, Thinking: r.thinking, Status: r.conn.Status})
	}
	return out, nil
}

type resolvedModel struct {
	conn      *Connection
	model     agent.ModelDef
	thinking  string
	inherited bool
}

// ErrNotConfigured is returned when there is no enabled connection or no default model.
func errNotConfigured() error {
	return apperr.Conflict("agent_not_configured", "the agent is not configured: an LLM connection and a default model are required")
}

// resolveModel: override(scenario) ?? default; a disabled connection in an
// override falls back to the default (arch §6, R8).
func (s *Service) resolveModel(ctx context.Context, sc agent.Scenario) (*resolvedModel, error) {
	sm, err := s.scenarioModels(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	try := func(m *ModelChoice, inherited bool) *resolvedModel {
		if m == nil {
			return nil
		}
		c, err := s.connection(ctx, m.ConnectionID)
		if err != nil || !c.Enabled {
			return nil
		}
		for _, md := range c.Models {
			if md.ID == m.Model {
				th := m.Thinking
				if !md.SupportsThinking(th) {
					th = "off"
				}
				return &resolvedModel{conn: c, model: md, thinking: th, inherited: inherited}
			}
		}
		return nil
	}
	if r := try(sm.Scenarios[sc], false); r != nil {
		return r, nil
	}
	if r := try(sm.Default, true); r != nil {
		return r, nil
	}
	return nil, errNotConfigured()
}

// ─── audit ──────────────────────────────────────────────────────────

func audit(ctx context.Context, q postgres.Querier, actor *uuid.UUID, objType, ref, action, summary string) error {
	_, err := q.Exec(ctx, `INSERT INTO agent_config_changes (object_type, object_ref, action, summary, actor_id) VALUES ($1,$2,$3,$4,$5)`,
		objType, ref, action, summary, actor)
	return err
}

// ─── bootstrap (tech spec §11) ──────────────────────────────────────

// Bootstrap creates the DeepSeek connection and the default model from
// BOOTSTRAP_DEEPSEEK_API_KEY once, when there are no connections. Replicas
// starting together serialize on an advisory lock (CON-11). baseURL, when set,
// replaces the preset address (BOOTSTRAP_DEEPSEEK_BASE_URL, demos with fakellm).
func (s *Service) Bootstrap(ctx context.Context, key, baseURL string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	pr := Presets[TypeDeepSeek]
	if baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/"); baseURL != "" {
		pr.BaseURL = baseURL
	}
	enc, err := s.box.Seal(key)
	if err != nil {
		return err
	}
	created := false
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('hammurapi.agent.bootstrap'))`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM llm_connections`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		id := uuid.New()
		mj, _ := json.Marshal(pr.Models)
		if _, err := tx.Exec(ctx, `INSERT INTO llm_connections (id, name, type, base_url, models, api_key_enc, api_key_last4)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, pr.Name, pr.Type, pr.BaseURL, mj, enc, last4(key)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_default_model (id, connection_id, model, thinking) VALUES (true,$1,'deepseek-v4-flash','off')
			ON CONFLICT (id) DO NOTHING`, id); err != nil {
			return err
		}
		created = true
		return audit(ctx, tx, nil, "connection", id.String(), "create", "connection \"DeepSeek\" created from BOOTSTRAP_DEEPSEEK_API_KEY; default model deepseek-v4-flash")
	})
	if created {
		slog.Info("agent bootstrap: created the DeepSeek connection and the default model; BOOTSTRAP_DEEPSEEK_API_KEY can be removed")
	}
	return err
}
