package agentcfg

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Routes mounts /admin/api/v1/agent; every method requires a global
// administrator (CON-02).
func (s *Service) Routes(r chi.Router) {
	r.Route("/agent", func(r chi.Router) {
		r.Use(globalOnly)
		r.Get("/connections", handle(func(ctx context.Context, p *domain.Principal, _ *http.Request) (any, error) {
			list, err := s.Connections(ctx, p)
			if err != nil {
				return nil, err
			}
			scs, err := s.ScenariosOn(ctx, p)
			if err != nil {
				return nil, err
			}
			return map[string]any{"items": list, "scenarios": scs}, nil
		}))
		r.Post("/connections", handleCode(http.StatusCreated, func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			var in ConnectionInput
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.CreateConnection(ctx, p, in)
		}))
		r.Post("/connections/check", handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			var in ConnectionInput
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.CheckDraft(ctx, p, in)
		}))
		r.Patch("/connections/{id}", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, r *http.Request) (any, error) {
			var in ConnectionPatch
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.UpdateConnection(ctx, p, id, in)
		}))
		r.Put("/connections/{id}/key", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, r *http.Request) (any, error) {
			var in struct {
				APIKey string `json:"apiKey"`
			}
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			c, err := s.ReplaceKey(ctx, p, id, in.APIKey)
			if err != nil {
				return nil, err
			}
			return map[string]string{"keyLast4": c.KeyLast4}, nil
		}))
		r.Delete("/connections/{id}", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, _ *http.Request) (any, error) {
			return nil, s.DeleteConnection(ctx, p, id)
		}))
		r.Post("/connections/{id}/check", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, _ *http.Request) (any, error) {
			return s.CheckConnection(ctx, p, id)
		}))

		r.Get("/scenario-models", handle(func(ctx context.Context, p *domain.Principal, _ *http.Request) (any, error) {
			return s.GetScenarioModels(ctx, p)
		}))
		r.Put("/scenario-models", handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			var in ScenarioModels
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.PutScenarioModels(ctx, p, in)
		}))
		r.Get("/scenario-models/resolved", handle(func(ctx context.Context, p *domain.Principal, _ *http.Request) (any, error) {
			return s.ResolvedModels(ctx, p)
		}))

		r.Get("/skills", handle(func(ctx context.Context, p *domain.Principal, _ *http.Request) (any, error) { return s.Skills(ctx, p) }))
		r.Post("/skills", handleCode(http.StatusAccepted, func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			sk, scs, err := parseSkillUpload(r)
			if err != nil {
				return nil, err
			}
			c, err := s.AddSkill(ctx, p, sk, scs)
			if err != nil {
				return nil, err
			}
			return map[string]any{"changeId": c.ID, "prUrl": c.PRURL}, nil
		}))
		r.Get("/skills/changes", handle(func(ctx context.Context, p *domain.Principal, _ *http.Request) (any, error) {
			return s.SkillChanges(ctx, p)
		}))
		r.Post("/skills/changes/{id}/approve", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, _ *http.Request) (any, error) {
			return s.ApproveSkill(ctx, p, id)
		}))
		r.Post("/skills/changes/{id}/withdraw", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, _ *http.Request) (any, error) {
			return s.WithdrawSkill(ctx, p, id)
		}))
		r.Put("/skills/{name}/scenarios", handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			var in struct {
				Scenarios []agent.Scenario `json:"scenarios"`
			}
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.SetSkillScenarios(ctx, p, chi.URLParam(r, "name"), in.Scenarios)
		}))
		r.Delete("/skills/{name}", handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			return s.DeleteSkill(ctx, p, chi.URLParam(r, "name"))
		}))

		r.Get("/mcp-servers", handle(func(ctx context.Context, p *domain.Principal, _ *http.Request) (any, error) {
			return s.MCPServers(ctx, p)
		}))
		r.Post("/mcp-servers", handleCode(http.StatusCreated, func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			var in MCPInput
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.CreateMCP(ctx, p, in)
		}))
		r.Post("/mcp-servers/check", handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			var in MCPInput
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.CheckMCPDraft(ctx, p, in)
		}))
		// The built-in server has no id: any attempt to address it by name is refused (MCP-06).
		r.Patch("/mcp-servers/hammurapi", builtinRefused)
		r.Delete("/mcp-servers/hammurapi", builtinRefused)
		r.Patch("/mcp-servers/{id}", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, r *http.Request) (any, error) {
			var in MCPInput
			if err := httpx.Decode(r, &in); err != nil {
				return nil, err
			}
			return s.UpdateMCP(ctx, p, id, in)
		}))
		r.Delete("/mcp-servers/{id}", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, _ *http.Request) (any, error) {
			return nil, s.DeleteMCP(ctx, p, id)
		}))
		r.Post("/mcp-servers/{id}/check", withID(func(ctx context.Context, p *domain.Principal, id uuid.UUID, _ *http.Request) (any, error) {
			return s.CheckMCP(ctx, p, id)
		}))

		r.Get("/usage", handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			q := r.URL.Query()
			to := time.Now()
			from := to.AddDate(0, 0, -7)
			var err error
			if v := q.Get("from"); v != "" {
				if from, err = time.Parse(time.RFC3339, v); err != nil {
					return nil, apperr.BadRequest("invalid_period", "from must be RFC 3339")
				}
			}
			if v := q.Get("to"); v != "" {
				if to, err = time.Parse(time.RFC3339, v); err != nil {
					return nil, apperr.BadRequest("invalid_period", "to must be RFC 3339")
				}
			}
			var groups []string
			for _, g := range strings.Split(q.Get("groupBy"), ",") {
				if g = strings.TrimSpace(g); g != "" {
					groups = append(groups, g)
				}
			}
			return s.Usage(ctx, p, from, to, groups)
		}))
		r.Get("/audit", handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
			page, err := httpx.ParsePage(r)
			if err != nil {
				return nil, err
			}
			return s.Audit(ctx, p, page)
		}))
	})
}

func globalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := httpx.PrincipalFrom(r.Context()); p == nil || !p.GlobalAdmin {
			httpx.Error(w, r, apperr.Forbidden("forbidden", "the Agent section is available to global administrators only"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func builtinRefused(w http.ResponseWriter, r *http.Request) {
	httpx.Error(w, r, apperr.Conflict("mcp_builtin", "the built-in hammurapi MCP server cannot be changed or deleted"))
}

func handleCode(code int, fn func(context.Context, *domain.Principal, *http.Request) (any, error)) http.HandlerFunc {
	return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		res, err := fn(r.Context(), p, r)
		if err != nil {
			return err
		}
		if res == nil {
			httpx.NoContent(w)
			return nil
		}
		httpx.JSON(w, code, res)
		return nil
	})
}

func handle(fn func(context.Context, *domain.Principal, *http.Request) (any, error)) http.HandlerFunc {
	return handleCode(http.StatusOK, fn)
}

func withID(fn func(context.Context, *domain.Principal, uuid.UUID, *http.Request) (any, error)) http.HandlerFunc {
	return handle(func(ctx context.Context, p *domain.Principal, r *http.Request) (any, error) {
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return nil, err
		}
		return fn(ctx, p, id, r)
	})
}

// parseSkillUpload reads multipart/form-data: archive (zip) or skillMd, and scenarios.
func parseSkillUpload(r *http.Request) (*ParsedSkill, []agent.Scenario, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxSkillZip+(1<<20))
	if err := r.ParseMultipartForm(maxSkillZip + (1 << 20)); err != nil {
		return nil, nil, invalid("the upload is larger than 5 MB or not multipart/form-data")
	}
	var scs []agent.Scenario
	for _, v := range r.MultipartForm.Value["scenarios"] {
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				scs = append(scs, agent.Scenario(x))
			}
		}
	}
	if f, _, err := r.FormFile("archive"); err == nil {
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxSkillZip+1))
		if err != nil {
			return nil, nil, invalid("cannot read the archive")
		}
		sk, err := ParseSkillZip(data)
		return sk, scs, err
	}
	if md := r.FormValue("skillMd"); md != "" {
		sk, err := ParseSkillText(md)
		return sk, scs, err
	}
	return nil, nil, invalid("send a zip archive (archive) or the text of SKILL.md (skillMd)")
}
