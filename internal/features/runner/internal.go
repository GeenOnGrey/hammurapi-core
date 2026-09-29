// Package runner implements agent tasks in an isolated environment
// (PLT.HMR-0002 arch §7): the internal API on :8081 that runner processes
// talk to with a one-time task token, and the runner mode itself
// (`hammurapi runner --task <id>`): check out the service repository through
// the provider API, run the agent with files and terminal confined to the
// working directory, commit as the bot and open or update the PR.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GeenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// Description is GET /internal/v1/tasks/{id}: everything the runner needs.
type Description struct {
	ID             uuid.UUID               `json:"id"`
	Type           string                  `json:"type"`
	Provider       string                  `json:"provider"`
	GitBaseURL     string                  `json:"gitBaseUrl"`
	Repo           string                  `json:"repo"`
	Branch         string                  `json:"branch"`
	Feature        string                  `json:"feature"`
	FeatureTitle   string                  `json:"featureTitle"`
	Service        string                  `json:"service"`
	Autonomy       domain.Autonomy         `json:"autonomy"`
	Initiator      string                  `json:"initiator"`
	Reviewers      []string                `json:"reviewers"`
	Requirements   []cycledata.Requirement `json:"requirements"`
	TestCases      []cycledata.TestCase    `json:"testCases"`
	Specs          map[string]string       `json:"specs"`
	Input          codegen.TaskInput       `json:"input"`
	Release        string                  `json:"release,omitempty"`
	TimeoutSeconds int                     `json:"timeoutSeconds"`
	TokenLimit     int64                   `json:"tokenLimit"`
	MCPURL         string                  `json:"mcpUrl"`
}

// Result is POST /internal/v1/tasks/{id}/result.
type Result struct {
	Status       string   `json:"status"` // succeeded | failed
	PRNumber     int      `json:"prNumber,omitempty"`
	PRURL        string   `json:"prUrl,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	HeadSHA      string   `json:"headSha,omitempty"`
	Requirements []string `json:"requirements"`
	TestCases    []string `json:"testCases"`
	Summary      string   `json:"summary"`
	Plan         string   `json:"plan,omitempty"`
	Error        string   `json:"error,omitempty"`
	TokensIn     int64    `json:"tokensIn"`
	TokensOut    int64    `json:"tokensOut"`
}

// Progress is POST /internal/v1/tasks/{id}/progress.
type Progress struct {
	Message   string `json:"message"`
	TokensIn  int64  `json:"tokensIn"`
	TokensOut int64  `json:"tokensOut"`
}

// Internal serves the internal API.
type Internal struct {
	Pool       *pgxpool.Pool
	Store      specdata.Store
	Git        git.Provider
	Events     events.Publisher
	MCP        *mcp.Server
	GitBaseURL string
	PublicURL  string // internal URL of this server as seen by runners
	Timeout    time.Duration
	TokenLimit int64
}

type ctxKey struct{}

// auth resolves the task token (RUN-06: a finished task's token is revoked).
func (s *Internal) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		t, err := cycledata.New(s.Pool).TaskByTokenHash(r.Context(), codegen.HashToken(tok))
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		if id := chi.URLParam(r, "id"); id != "" && id != t.ID.String() {
			http.Error(w, "token of another task", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, t)))
	})
}

func taskOf(r *http.Request) *cycledata.Task { return r.Context().Value(ctxKey{}).(*cycledata.Task) }

// ResolveMCP resolves a task token to an MCP grant (the runner's agent session).
func (s *Internal) ResolveMCP(ctx context.Context, tok string) (mcp.Grant, bool) {
	t, err := cycledata.New(s.Pool).TaskByTokenHash(ctx, codegen.HashToken(tok))
	if err != nil {
		return mcp.Grant{}, false
	}
	g := mcp.Grant{Mode: mcp.ModeTask, Subject: t.ID}
	if t.InitiatorID != nil {
		g.UserID = *t.InitiatorID
	}
	if t.FeatureID != nil {
		if f, err := s.Store.FeatureByID(ctx, *t.FeatureID); err == nil {
			g.Feature, g.ContextType, g.ContextKey = f.UniqueID, "feature", f.UniqueID
		}
	}
	id := t.ID
	g.Sink = func(kind string, payload json.RawMessage) error {
		if kind != "progress" {
			return nil
		}
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &p)
		return s.progress(context.WithoutCancel(ctx), id, Progress{Message: p.Message})
	}
	return g, true
}

// Routes mounts /internal/v1 on the internal server.
func (s *Internal) Routes(r chi.Router) {
	r.Route("/internal/v1/tasks/{id}", func(r chi.Router) {
		r.Use(s.auth)
		r.Get("/", httpx.Handler(s.describe))
		r.Post("/git-token", httpx.Handler(s.gitToken))
		r.Post("/progress", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			var p Progress
			if err := httpx.Decode(r, &p); err != nil {
				return err
			}
			if err := s.progress(r.Context(), taskOf(r).ID, p); err != nil {
				return err
			}
			httpx.NoContent(w)
			return nil
		}))
		r.Post("/result", httpx.Handler(s.result))
	})
	r.Handle("/internal/v1/mcp", s.MCP)
}

func (s *Internal) progress(ctx context.Context, id uuid.UUID, p Progress) error {
	cd := cycledata.New(s.Pool)
	if err := cd.TaskProgress(ctx, id, truncate(p.Message, 500), p.TokensIn, p.TokensOut); err != nil {
		return err
	}
	t, err := cd.TaskByID(ctx, id)
	if err != nil {
		return err
	}
	s.Events.Publish(ctx, events.Event{Type: events.TaskProgress, Data: map[string]any{"taskId": id, "service": t.Service, "message": p.Message,
		"tokensIn": t.TokensIn, "tokensOut": t.TokensOut, "status": t.Status}})
	return nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (s *Internal) describe(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t := taskOf(r)
	cd := cycledata.New(s.Pool)
	svc, err := cd.ServiceByID(ctx, t.ServiceID)
	if err != nil {
		return err
	}
	d := &Description{ID: t.ID, Type: t.Type, Provider: s.Git.Name(), GitBaseURL: s.GitBaseURL, Repo: svc.Repo, Service: svc.Key,
		Autonomy: svc.Autonomy, Reviewers: svc.Owners, Specs: map[string]string{}, Requirements: []cycledata.Requirement{},
		TestCases: []cycledata.TestCase{}, TimeoutSeconds: int(s.Timeout.Seconds()), TokenLimit: s.TokenLimit,
		MCPURL: strings.TrimRight(s.PublicURL, "/") + "/internal/v1/mcp"}
	if d.Reviewers == nil {
		d.Reviewers = []string{}
	}
	_ = json.Unmarshal(t.Input, &d.Input)
	if t.InitiatorID != nil {
		d.Initiator, _ = cd.Username(ctx, *t.InitiatorID)
	}
	if t.FeatureID != nil {
		f, err := s.Store.FeatureByID(ctx, *t.FeatureID)
		if err != nil {
			return err
		}
		d.Feature, d.FeatureTitle = f.UniqueID, f.Title
		d.Branch = git.ServiceBranch(f.UniqueID, svc.Key)
		reqs, err := cd.Requirements(ctx, f.ID)
		if err != nil {
			return err
		}
		mine := map[string]bool{}
		for _, rq := range reqs {
			for _, x := range rq.Services {
				if x == svc.Key {
					d.Requirements = append(d.Requirements, rq)
					mine[rq.ID] = true
				}
			}
		}
		tcs, err := cd.TestCases(ctx, f.ID)
		if err != nil {
			return err
		}
		for _, tc := range tcs {
			for _, id := range tc.ReqIDs {
				if mine[id] {
					d.TestCases = append(d.TestCases, tc)
					break
				}
			}
		}
		if token, err := s.Git.BotToken(ctx); err == nil {
			for _, a := range []domain.Area{domain.AreaProduct, domain.AreaArch, domain.AreaTech, domain.AreaQA} {
				if file, err := s.Git.GetFile(ctx, token, f.Branch, git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, string(a))); err == nil {
					d.Specs[string(a)] = string(file.Content)
				}
			}
		}
	}
	if t.ReleaseID != nil {
		if rel, err := cd.ReleaseByID(ctx, *t.ReleaseID); err == nil {
			d.Release = rel.Key
			if t.Type == codegen.TaskRevert {
				d.Branch = git.RevertBranch(rel.Key, svc.Key)
			}
		}
	}
	httpx.JSON(w, 200, d)
	return nil
}

// gitToken returns a bot installation token limited to the task's repository (RUN-07).
func (s *Internal) gitToken(w http.ResponseWriter, r *http.Request) error {
	t := taskOf(r)
	svc, err := cycledata.New(s.Pool).ServiceByID(r.Context(), t.ServiceID)
	if err != nil {
		return err
	}
	tok, err := s.Git.ForRepo(svc.Repo).BotToken(r.Context())
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, map[string]any{"token": tok, "repo": svc.Repo, "expiresAt": time.Now().Add(55 * time.Minute)})
	return nil
}

func (s *Internal) result(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t := taskOf(r)
	var res Result
	if err := httpx.Decode(r, &res); err != nil {
		return err
	}
	if res.Status != "succeeded" {
		res.Status = "failed"
	}
	raw, _ := json.Marshal(res)
	err := postgres.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		cd := cycledata.New(tx)
		var errText *string
		if res.Error != "" {
			errText = &res.Error
		}
		if err := cd.TaskProgress(ctx, t.ID, truncate(res.Summary, 500), res.TokensIn, res.TokensOut); err != nil {
			return err
		}
		if err := cd.FinishTask(ctx, t.ID, res.Status, raw, errText); err != nil {
			return err
		}
		if err := cd.AddUsage(ctx, cycledata.Usage{Context: "codegen", FeatureID: t.FeatureID, ReleaseID: t.ReleaseID, TaskID: &t.ID,
			UserID: t.InitiatorID, TokensIn: res.TokensIn, TokensOut: res.TokensOut}); err != nil {
			return err
		}
		if res.PRNumber > 0 && t.FeatureID != nil {
			svc, err := cd.ServiceByID(ctx, t.ServiceID)
			if err != nil {
				return err
			}
			kind := "service"
			if t.Type == codegen.TaskRevert {
				kind = "revert"
			}
			pr, err := cd.PRByRepoNumber(ctx, svc.Repo, res.PRNumber)
			if errors.Is(err, cycledata.ErrNotFound) {
				pr = &cycledata.PR{Repo: svc.Repo, Number: res.PRNumber, Kind: kind, FeatureID: *t.FeatureID, ServiceID: &t.ServiceID,
					ByAgent: true, State: "open", Review: "required"}
				if svc.Autonomy == domain.AutonomyAutonomous {
					pr.Review = "not_required"
				}
			} else if err != nil {
				return err
			}
			pr.URL, pr.Branch, pr.HeadSHA = res.PRURL, res.Branch, res.HeadSHA
			if pr.Title == "" {
				pr.Title = res.Summary
			}
			if kind == "revert" {
				var in codegen.TaskInput
				_ = json.Unmarshal(t.Input, &in)
				if id, err := uuid.Parse(in.RevertPRID); err == nil {
					pr.RevertsPRID = &id
				}
			}
			if err := cd.UpsertPR(ctx, pr); err != nil {
				return err
			}
			if res.HeadSHA != "" {
				if err := cd.SetPRHead(ctx, pr.ID, res.HeadSHA); err != nil {
					return err
				}
			}
			if len(res.Requirements) > 0 {
				if err := cd.SetPRRequirements(ctx, pr.ID, res.Requirements); err != nil {
					return err
				}
			}
		}
		return workflows.Send(ctx, tx, t.RunID, "task_result", map[string]any{"status": res.Status, "error": res.Error, "prNumber": res.PRNumber})
	})
	if err != nil {
		return err
	}
	metrics.RunnerTokens.WithLabelValues(t.Type).Add(float64(res.TokensIn + res.TokensOut))
	s.Events.Publish(ctx, events.Event{Type: events.TaskProgress, Data: map[string]any{"taskId": t.ID, "status": res.Status, "service": t.Service}})
	httpx.NoContent(w)
	return nil
}
