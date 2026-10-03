package agentcfg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// BuiltinMCP is the name of Hammurapi's own MCP server (R17).
const BuiltinMCP = "hammurapi"

var (
	mcpName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)
	headerName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// HeaderMeta is a header of an MCP server as the API shows it.
type HeaderMeta struct {
	Name  string `json:"name"`
	Last4 string `json:"last4"`
}

// MCPServer is an MCP server as the API shows it (never header values).
type MCPServer struct {
	ID           *uuid.UUID       `json:"id"`
	Name         string           `json:"name"`
	URL          string           `json:"url"`
	Headers      []HeaderMeta     `json:"headers"`
	Exposure     string           `json:"exposure"`
	Scenarios    []agent.Scenario `json:"scenarios"`
	Enabled      bool             `json:"enabled"`
	Builtin      bool             `json:"builtin"`
	Status       string           `json:"status"`
	StatusReason *string          `json:"statusReason"`
	ToolsCount   *int             `json:"toolsCount"`
	StatusAt     *time.Time       `json:"statusAt"`
}

func builtin() MCPServer {
	return MCPServer{Name: BuiltinMCP, URL: "", Headers: []HeaderMeta{}, Exposure: "direct", Scenarios: agent.Scenarios,
		Enabled: true, Builtin: true, Status: "ok"}
}

const mcpSelect = `SELECT id, name, url, header_meta, exposure, scenarios::text[], enabled, status, status_reason, tools_count, status_at FROM mcp_servers`

func scanMCP(row pgx.Row) (*MCPServer, error) {
	var m MCPServer
	var id uuid.UUID
	var meta []byte
	var scs []string
	if err := row.Scan(&id, &m.Name, &m.URL, &meta, &m.Exposure, &scs, &m.Enabled, &m.Status, &m.StatusReason, &m.ToolsCount, &m.StatusAt); err != nil {
		return nil, err
	}
	m.ID = &id
	_ = json.Unmarshal(meta, &m.Headers)
	if m.Headers == nil {
		m.Headers = []HeaderMeta{}
	}
	m.Scenarios = make([]agent.Scenario, 0, len(scs))
	for _, s := range scs {
		m.Scenarios = append(m.Scenarios, agent.Scenario(s))
	}
	if !m.Enabled {
		m.Status = "disabled"
	}
	return &m, nil
}

// MCPServers lists the servers, the built-in one first.
func (s *Service) MCPServers(ctx context.Context, p *domain.Principal) ([]MCPServer, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, mcpSelect+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPServer{builtin()}
	for rows.Next() {
		m, err := scanMCP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Service) mcpServer(ctx context.Context, id uuid.UUID) (*MCPServer, error) {
	m, err := scanMCP(s.pool.QueryRow(ctx, mcpSelect+` WHERE id = $1`, id))
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("mcp_not_found", "MCP server not found")
	}
	return m, err
}

// HeaderInput is a header in the request; Value empty keeps the stored value.
type HeaderInput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPInput is the body of POST /mcp-servers, PATCH /mcp-servers/{id}, POST /mcp-servers/check.
type MCPInput struct {
	Name      string           `json:"name"`
	URL       string           `json:"url"`
	Headers   []HeaderInput    `json:"headers"`
	Exposure  string           `json:"exposure"`
	Scenarios []agent.Scenario `json:"scenarios"`
	Enabled   *bool            `json:"enabled"`
}

// validURL: https, or http only for in-cluster addresses (R18, MCP-08).
func validURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return apperr.Unprocessable("mcp_invalid", "the URL is not valid").With("field", "url")
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && (strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")):
	default:
		return apperr.Unprocessable("mcp_invalid", "only https:// (or http:// to a *.svc address in the cluster) is supported; stdio and SSE servers are not").With("field", "url")
	}
	if strings.HasSuffix(u.Path, "/sse") {
		return apperr.Unprocessable("mcp_invalid", "the legacy SSE transport is not supported; use the streamable HTTP endpoint").With("field", "url")
	}
	return nil
}

func (in *MCPInput) validate(creating bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if creating || in.Name != "" {
		if in.Name == BuiltinMCP {
			return apperr.Conflict("mcp_builtin", "the built-in hammurapi server cannot be changed")
		}
		if !mcpName.MatchString(in.Name) {
			return apperr.Unprocessable("mcp_invalid", "the name must be 2–41 lower-case letters, digits and dashes").With("field", "name")
		}
	}
	if creating || in.URL != "" {
		if err := validURL(in.URL); err != nil {
			return err
		}
	}
	if in.Exposure == "" && creating {
		in.Exposure = "deferred" // tools found by search: the default (R15)
	}
	if in.Exposure != "" && in.Exposure != "direct" && in.Exposure != "deferred" {
		return apperr.Unprocessable("mcp_invalid", "exposure must be direct or deferred").With("field", "exposure")
	}
	if len(in.Headers) > 10 {
		return apperr.Unprocessable("mcp_invalid", "at most 10 headers").With("field", "headers")
	}
	seen := map[string]bool{}
	for _, h := range in.Headers {
		if !headerName.MatchString(h.Name) || seen[strings.ToLower(h.Name)] {
			return apperr.Unprocessable("mcp_invalid", "invalid or duplicate header name "+h.Name).With("field", "headers")
		}
		seen[strings.ToLower(h.Name)] = true
	}
	for _, sc := range in.Scenarios {
		if !sc.Valid() {
			return apperr.Unprocessable("mcp_invalid", "unknown scenario "+string(sc)).With("field", "scenarios")
		}
	}
	return nil
}

// headerValues decrypts the stored headers of a server.
func (s *Service) headerValues(ctx context.Context, q postgres.Querier, id uuid.UUID) (map[string]string, error) {
	var enc []byte
	if err := q.QueryRow(ctx, `SELECT headers_enc FROM mcp_servers WHERE id = $1`, id).Scan(&enc); err != nil {
		return nil, err
	}
	vals := map[string]string{}
	if len(enc) == 0 {
		return vals, nil
	}
	plain, err := s.box.Open(enc)
	if err != nil {
		return nil, err
	}
	return vals, json.Unmarshal([]byte(plain), &vals)
}

// mergeHeaders applies the input to the stored values: a header without a
// value keeps its stored value; headers not in the input are removed.
func mergeHeaders(stored map[string]string, in []HeaderInput) (map[string]string, error) {
	out := map[string]string{}
	for _, h := range in {
		v := h.Value
		if v == "" {
			v = stored[h.Name]
		}
		if v == "" {
			return nil, apperr.Unprocessable("mcp_invalid", "header "+h.Name+" needs a value").With("field", "headers")
		}
		out[h.Name] = v
	}
	return out, nil
}

func (s *Service) sealHeaders(h map[string]string) ([]byte, []byte, error) {
	meta := []HeaderMeta{}
	for name, v := range h {
		meta = append(meta, HeaderMeta{Name: name, Last4: last4(v)})
	}
	mj, _ := json.Marshal(meta)
	if len(h) == 0 {
		return nil, mj, nil
	}
	plain, _ := json.Marshal(h)
	enc, err := s.box.Seal(string(plain))
	return enc, mj, err
}

func scenarioNames(scs []agent.Scenario) []string {
	out := make([]string, len(scs))
	for i, s := range scs {
		out[i] = string(s)
	}
	return out
}

// CreateMCP adds a server (R15).
func (s *Service) CreateMCP(ctx context.Context, p *domain.Principal, in MCPInput) (*MCPServer, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if err := in.validate(true); err != nil {
		return nil, err
	}
	vals, err := mergeHeaders(nil, in.Headers)
	if err != nil {
		return nil, err
	}
	enc, meta, err := s.sealHeaders(vals)
	if err != nil {
		return nil, err
	}
	enabled := in.Enabled == nil || *in.Enabled
	id := uuid.New()
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_servers (id, name, url, headers_enc, header_meta, exposure, scenarios, enabled, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7::text[]::agent_scenario[],$8,$9)`, id, in.Name, strings.TrimSpace(in.URL), enc, meta, in.Exposure,
			scenarioNames(in.Scenarios), enabled, p.UserID); err != nil {
			if postgres.IsUniqueViolation(err) {
				return apperr.Conflict("mcp_name_taken", "an MCP server with this name already exists").With("field", "name")
			}
			return err
		}
		return audit(ctx, tx, &p.UserID, "mcp_server", in.Name, "create",
			fmt.Sprintf("MCP server %q %s, %s, scenarios %s", in.Name, in.URL, in.Exposure, strings.Join(scenarioNames(in.Scenarios), ", ")))
	})
	if err != nil {
		return nil, err
	}
	return s.mcpServer(ctx, id)
}

// UpdateMCP changes a server; headers without a value keep their value.
func (s *Service) UpdateMCP(ctx context.Context, p *domain.Principal, id uuid.UUID, in MCPInput) (*MCPServer, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	cur, err := s.mcpServer(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := in.validate(false); err != nil {
		return nil, err
	}
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		name, u := cur.Name, cur.URL
		if in.Name != "" {
			name = in.Name
		}
		if in.URL != "" {
			u = strings.TrimSpace(in.URL)
		}
		enabled := cur.Enabled
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		scs := cur.Scenarios
		if in.Scenarios != nil {
			scs = in.Scenarios
		}
		exposure := cur.Exposure
		if in.Exposure != "" {
			exposure = in.Exposure
		}
		args := []any{id, name, u, exposure, scenarioNames(scs), enabled}
		q := `UPDATE mcp_servers SET name = $2, url = $3, exposure = $4, scenarios = $5::text[]::agent_scenario[], enabled = $6, updated_at = now()`
		if in.Headers != nil {
			stored, err := s.headerValues(ctx, tx, id)
			if err != nil {
				return err
			}
			vals, err := mergeHeaders(stored, in.Headers)
			if err != nil {
				return err
			}
			enc, meta, err := s.sealHeaders(vals)
			if err != nil {
				return err
			}
			args = append(args, enc, meta)
			q += `, headers_enc = $7, header_meta = $8, status = 'unknown', status_reason = NULL`
		}
		if _, err := tx.Exec(ctx, q+` WHERE id = $1`, args...); err != nil {
			if postgres.IsUniqueViolation(err) {
				return apperr.Conflict("mcp_name_taken", "an MCP server with this name already exists").With("field", "name")
			}
			return err
		}
		return audit(ctx, tx, &p.UserID, "mcp_server", name, "update",
			fmt.Sprintf("MCP server %q: %s, enabled %t, scenarios %s", name, exposure, enabled, strings.Join(scenarioNames(scs), ", ")))
	})
	if err != nil {
		return nil, err
	}
	s.publishFocus(ctx)
	return s.mcpServer(ctx, id)
}

// DeleteMCP removes a server.
func (s *Service) DeleteMCP(ctx context.Context, p *domain.Principal, id uuid.UUID) error {
	if err := requireGlobal(p); err != nil {
		return err
	}
	cur, err := s.mcpServer(ctx, id)
	if err != nil {
		return err
	}
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM mcp_servers WHERE id = $1`, id); err != nil {
			return err
		}
		return audit(ctx, tx, &p.UserID, "mcp_server", cur.Name, "delete", fmt.Sprintf("MCP server %q deleted", cur.Name))
	})
	if err == nil {
		s.publishFocus(ctx)
	}
	return err
}

// CheckMCP checks a saved server and stores its status (R16).
func (s *Service) CheckMCP(ctx context.Context, p *domain.Principal, id uuid.UUID) (*agent.MCPCheckResponse, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	cur, err := s.mcpServer(ctx, id)
	if err != nil {
		return nil, err
	}
	vals, err := s.headerValues(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	res, err := s.checkMCP(ctx, cur.Name, cur.URL, cur.Exposure, vals)
	if err != nil {
		return nil, err
	}
	status, reason := "ok", ""
	var tools *int
	if res.OK {
		n := len(res.Tools)
		tools = &n
	} else {
		status, reason = "error", res.Error
	}
	_, _ = s.pool.Exec(ctx, `UPDATE mcp_servers SET status = $2, status_reason = NULLIF($3,''), tools_count = $4, status_at = now() WHERE id = $1`,
		id, status, reason, tools)
	s.publishFocus(ctx)
	return res, nil
}

// CheckMCPDraft checks a server that is not saved yet.
func (s *Service) CheckMCPDraft(ctx context.Context, p *domain.Principal, in MCPInput) (*agent.MCPCheckResponse, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if in.Name == "" {
		in.Name = "draft"
	}
	if err := in.validate(true); err != nil {
		return nil, err
	}
	vals, err := mergeHeaders(nil, in.Headers)
	if err != nil {
		return nil, err
	}
	return s.checkMCP(ctx, in.Name, in.URL, in.Exposure, vals)
}

func (s *Service) checkMCP(ctx context.Context, name, u, exposure string, headers map[string]string) (*agent.MCPCheckResponse, error) {
	if s.operator == nil {
		return nil, apperr.Unavailable("agent_unavailable", "the agent operator is not configured")
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	res, err := s.operator.CheckMCP(ctx, agent.MCPCheckRequest{Server: agent.MCPServer{Name: name, URL: u, HeaderNames: names, Exposure: exposure}, Headers: headers})
	if err != nil {
		return nil, operatorError(err)
	}
	return &res, nil
}
