// Package deploy starts deploy pipelines and receives their results
// (PLT.HMR-0002 R28, R40, arch §12): per-environment settings in the admin
// panel with per-service overrides, the deploy.trigger effect, the signed
// result webhook POST /hooks/v1/deploy and manual marks. Hammurapi does not
// deploy itself: it starts the pipeline of the team and waits for the result.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/cicd"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/signing"
)

// Effect is the deploy trigger effect.
const Effect = "deploy.trigger"

// EnvSettings are the settings of one environment.
type EnvSettings struct {
	Environment string        `json:"environment"`
	Settings    cicd.Settings `json:"settings"`
	SecretRefs  []string      `json:"-"`
	Secrets     int           `json:"activeSecrets"`
	UpdatedAt   *time.Time    `json:"updatedAt"`
}

// Load reads the settings of an environment; ok is false when not configured (DEP-07).
func Load(ctx context.Context, q postgres.Querier, env string) (*EnvSettings, bool, error) {
	var s EnvSettings
	var cfg []byte
	var timeout int
	var at time.Time
	err := q.QueryRow(ctx, `SELECT environment::text, type, config, secret_refs, timeout_minutes, updated_at FROM deploy_settings WHERE environment = $1::deploy_env`, env).
		Scan(&s.Environment, &s.Settings.Type, &cfg, &s.SecretRefs, &timeout, &at)
	if postgres.IsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	_ = json.Unmarshal(cfg, &s.Settings)
	s.Settings.TimeoutMinutes = timeout
	s.Secrets = len(s.SecretRefs)
	s.UpdatedAt = &at
	return &s, true, nil
}

// Timeout of the result wait.
func (s *EnvSettings) Timeout() time.Duration {
	if s == nil || s.Settings.TimeoutMinutes <= 0 {
		return 60 * time.Minute
	}
	return time.Duration(s.Settings.TimeoutMinutes) * time.Minute
}

// Override returns the service override of an environment.
func Override(svc *cycledata.Service, env string) *cicd.Override {
	if len(svc.DeployOverride) == 0 {
		return nil
	}
	var all map[string]*cicd.Override
	if json.Unmarshal(svc.DeployOverride, &all) != nil {
		return nil
	}
	return all[env]
}

// Deliver sends a finished (or started) deploy run to the workflow runs that wait for it.
func Deliver(ctx context.Context, q postgres.Querier, r *cycledata.DeployRun) error {
	payload := map[string]any{"deployRunId": r.ID, "serviceId": r.ServiceID, "service": r.Service, "status": r.Status, "environment": r.Environment}
	if r.ReleaseID != nil {
		for _, kind := range []string{"release", "rollback"} {
			if _, err := workflows.SendToSubject(ctx, q, kind, *r.ReleaseID, "deploy_result", payload); err != nil {
				return err
			}
		}
	}
	if r.FeatureID != nil && r.Environment == "stage" {
		if _, err := workflows.SendToSubject(ctx, q, "validation", *r.FeatureID, "deploy_result", payload); err != nil {
			return err
		}
	}
	return nil
}

// Mark records a manual mark "released" (production) or "deployed to stage".
func Mark(ctx context.Context, q postgres.Querier, runID uuid.UUID, version string, by uuid.UUID) (bool, error) {
	cd := cycledata.New(q)
	var v *string
	if version != "" {
		v = &version
	}
	changed, err := cd.UpdateDeployRun(ctx, runID, "success", "manual", v, nil, nil, &by)
	if err != nil || !changed {
		return changed, err
	}
	r, err := cd.DeployRunByID(ctx, runID)
	if err != nil {
		return false, err
	}
	metrics.DeployRuns.WithLabelValues(r.Environment, "success").Inc()
	return true, Deliver(ctx, q, r)
}

// ─── Trigger effect ─────────────────────────────────────────────────

// Effects starts pipelines (worker).
type Effects struct {
	Q         postgres.Querier
	Trigger   *cicd.Trigger
	Secrets   *signing.Secrets
	PublicURL string // base URL of /hooks/v1/deploy
}

// TriggerPayload is the effect payload.
type TriggerPayload struct {
	DeployRunID uuid.UUID `json:"deployRunId"`
	RunE2E      bool      `json:"runE2e,omitempty"`
}

// Do implements deploy.trigger (DEP-01…03).
func (e *Effects) Do(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in TriggerPayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	cd := cycledata.New(e.Q)
	run, err := cd.DeployRunByID(ctx, in.DeployRunID)
	if err != nil {
		return nil, err
	}
	if run.Status != "triggered" {
		return nil, nil // already has a result
	}
	set, ok, err := Load(ctx, e.Q, run.Environment)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil // not configured: tag or manual mark
	}
	svc, err := cd.ServiceByID(ctx, run.ServiceID)
	if err != nil {
		return nil, err
	}
	vars := cicd.Vars{Service: svc.Key, Repo: svc.Repo, Ref: run.Ref, Environment: run.Environment, RunID: run.ID.String(),
		CallbackURL: strings.TrimRight(e.PublicURL, "/") + "/hooks/v1/deploy"}
	if run.FeatureID != nil {
		_ = e.Q.QueryRow(ctx, `SELECT unique_id FROM features WHERE id = $1`, *run.FeatureID).Scan(&vars.Feature)
	}
	if run.ReleaseID != nil {
		_ = e.Q.QueryRow(ctx, `SELECT r.key, f.unique_id FROM releases r JOIN features f ON f.id = r.feature_id WHERE r.id = $1`, *run.ReleaseID).
			Scan(&vars.Release, &vars.Feature)
	}
	secret := ""
	if len(set.SecretRefs) > 0 {
		secret, _ = e.Secrets.Resolve(set.SecretRefs[0])
	}
	extra := map[string]string{}
	if in.RunE2E {
		extra["run_e2e"] = "true"
	}
	if run.IsRollback {
		extra["rollback"] = "true"
	}
	u, err := e.Trigger.Start(ctx, cicd.Request{Settings: set.Settings.Apply(Override(svc, run.Environment)), Vars: vars, Extra: extra, Secret: secret})
	if err != nil {
		return nil, fmt.Errorf("start deploy of %s: %w", svc.Key, err)
	}
	if u != "" {
		if err := cd.SetDeployRunURL(ctx, run.ID, u); err != nil {
			return nil, err
		}
	}
	metrics.DeployRuns.WithLabelValues(run.Environment, "triggered").Inc()
	return nil, nil
}

// ─── Result webhook ─────────────────────────────────────────────────

// HookPayload is POST /hooks/v1/deploy.
type HookPayload struct {
	RunID       string `json:"runId"`
	Service     string `json:"service"`
	Environment string `json:"environment"`
	Ref         string `json:"ref"`
	Status      string `json:"status"` // started | success | failure
	RunURL      string `json:"runUrl"`
}

// Hook serves POST /hooks/v1/deploy.
type Hook struct {
	Pool    *pgxpool.Pool
	Secrets *signing.Secrets
	Events  events.Publisher
	Now     func() time.Time
}

func (h *Hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	var p HookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	env := p.Environment
	if env != "stage" && env != "production" {
		http.Error(w, "environment must be stage or production", http.StatusBadRequest)
		return
	}
	set, ok, err := Load(ctx, h.Pool, env)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var secrets []string
	if ok {
		secrets = h.Secrets.ResolveAll(set.SecretRefs)
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if !signing.Verify(r.Header, body, secrets, now()) {
		metrics.WebhookEvents.WithLabelValues("unauthorized").Inc()
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(p.RunID)
	if err != nil {
		http.Error(w, "unknown runId", http.StatusNotFound) // HOOK-03
		return
	}
	status := map[string]string{"started": "started", "success": "success", "failure": "failure"}[p.Status]
	if status == "" {
		http.Error(w, "status must be started, success or failure", http.StatusBadRequest)
		return
	}
	var delivered *cycledata.DeployRun
	err = postgres.InTx(ctx, h.Pool, func(tx pgx.Tx) error {
		cd := cycledata.New(tx)
		run, err := cd.DeployRunByID(ctx, id)
		if err != nil {
			return err
		}
		var version, runURL, errText *string
		if p.Ref != "" {
			version = &p.Ref
		}
		if p.RunURL != "" {
			runURL = &p.RunURL
		}
		if status == "failure" {
			e := "the deploy pipeline failed"
			errText = &e
		}
		changed, err := cd.UpdateDeployRun(ctx, run.ID, status, "pipeline", version, runURL, errText, nil)
		if err != nil || !changed {
			return err // REL-09: the first terminal signal wins
		}
		if run, err = cd.DeployRunByID(ctx, run.ID); err != nil {
			return err
		}
		delivered = run
		return Deliver(ctx, tx, run)
	})
	if errors.Is(err, cycledata.ErrNotFound) {
		http.Error(w, "unknown runId", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "deploy webhook", "err", err)
		http.Error(w, "internal error", http.StatusServiceUnavailable)
		return
	}
	if delivered != nil {
		metrics.DeployRuns.WithLabelValues(delivered.Environment, delivered.Status).Inc()
		h.Events.Publish(ctx, events.Event{Type: events.ReleaseUpdated, Data: map[string]any{"deployRunId": delivered.ID, "status": delivered.Status}})
	}
	w.WriteHeader(http.StatusAccepted)
}

// ─── Admin API ──────────────────────────────────────────────────────

// Admin serves /admin/api/v1/deploy and service overrides.
type Admin struct {
	Pool    *pgxpool.Pool
	Secrets *signing.Secrets
	Effects *Effects
}

// Input is the body of PUT /admin/api/v1/deploy/{env}.
type Input struct {
	Type           string            `json:"type"`
	Workflow       string            `json:"workflow"`
	Ref            string            `json:"ref"`
	URL            string            `json:"url"`
	Auth           string            `json:"auth"`
	Params         map[string]string `json:"params"`
	TimeoutMinutes int               `json:"timeoutMinutes"`
}

func requireGlobal(p *domain.Principal) error {
	if !p.GlobalAdmin {
		return apperr.Forbidden("forbidden", "global administrator role required")
	}
	return nil
}

func envParam(r *http.Request) (string, error) {
	env := chi.URLParam(r, "env")
	if env != "stage" && env != "production" {
		return "", apperr.NotFound("unknown_environment", "environment must be stage or production")
	}
	return env, nil
}

// Put saves settings of an environment; the first save generates a result secret.
func (a *Admin) Put(ctx context.Context, p *domain.Principal, env string, in Input) (*EnvSettings, string, error) {
	if err := requireGlobal(p); err != nil {
		return nil, "", err
	}
	switch in.Type {
	case "github-actions", "gitlab-ci":
		if in.Workflow == "" && in.Type == "github-actions" {
			return nil, "", apperr.Unprocessable("workflow_required", "workflow file is required for GitHub Actions")
		}
	case "webhook":
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, "", apperr.Unprocessable("invalid_url", "a valid http(s) URL is required")
		}
	default:
		return nil, "", apperr.Unprocessable("invalid_type", "type must be github-actions, gitlab-ci or webhook")
	}
	if in.Auth != "secret" {
		in.Auth = "bot"
	}
	if in.TimeoutMinutes <= 0 {
		in.TimeoutMinutes = 60
	}
	cfg, _ := json.Marshal(cicd.Settings{Type: in.Type, Workflow: in.Workflow, Ref: in.Ref, URL: in.URL, Auth: in.Auth, Params: in.Params})
	current, ok, err := Load(ctx, a.Pool, env)
	if err != nil {
		return nil, "", err
	}
	refs := []string{}
	plain := ""
	if ok {
		refs = current.SecretRefs
	}
	if len(refs) == 0 {
		v, ref, err := a.Secrets.Generate()
		if err != nil {
			return nil, "", err
		}
		refs, plain = []string{ref}, v
	}
	if _, err := a.Pool.Exec(ctx, `INSERT INTO deploy_settings (environment, type, config, secret_refs, timeout_minutes, updated_by, updated_at)
		VALUES ($1::deploy_env, $2, $3, $4, $5, $6, now())
		ON CONFLICT (environment) DO UPDATE SET type = EXCLUDED.type, config = EXCLUDED.config, secret_refs = EXCLUDED.secret_refs,
			timeout_minutes = EXCLUDED.timeout_minutes, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		env, in.Type, cfg, refs, in.TimeoutMinutes, p.UserID); err != nil {
		return nil, "", err
	}
	s, _, err := Load(ctx, a.Pool, env)
	return s, plain, err
}

// Rotate adds a new result secret; the previous one stays active (HOOK-05).
func (a *Admin) Rotate(ctx context.Context, p *domain.Principal, env string) (string, error) {
	if err := requireGlobal(p); err != nil {
		return "", err
	}
	s, ok, err := Load(ctx, a.Pool, env)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", apperr.Conflict("not_configured", "configure the environment first")
	}
	v, ref, err := a.Secrets.Generate()
	if err != nil {
		return "", err
	}
	_, err = a.Pool.Exec(ctx, `UPDATE deploy_settings SET secret_refs = $2, updated_by = $3, updated_at = now() WHERE environment = $1::deploy_env`,
		env, signing.Rotate(s.SecretRefs, ref), p.UserID)
	return v, err
}

// Test starts a dry run of the pipeline for a service (DEP-06).
func (a *Admin) Test(ctx context.Context, p *domain.Principal, env, service string) (map[string]any, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	s, ok, err := Load(ctx, a.Pool, env)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.Conflict("not_configured", "configure the environment first")
	}
	svc, err := cycledata.New(a.Pool).ServiceByKey(ctx, service)
	if errors.Is(err, cycledata.ErrNotFound) {
		return nil, apperr.Unprocessable("unknown_service", "unknown service")
	}
	if err != nil {
		return nil, err
	}
	secret := ""
	if len(s.SecretRefs) > 0 {
		secret, _ = a.Secrets.Resolve(s.SecretRefs[0])
	}
	u, err := a.Effects.Trigger.Start(ctx, cicd.Request{Settings: s.Settings.Apply(Override(svc, env)),
		Vars: cicd.Vars{Service: svc.Key, Repo: svc.Repo, Ref: "main", Environment: env, RunID: "dry-run-" + uuid.NewString(),
			CallbackURL: strings.TrimRight(a.Effects.PublicURL, "/") + "/hooks/v1/deploy"},
		Extra: map[string]string{"dryRun": "true"}, Secret: secret})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	return map[string]any{"ok": true, "runUrl": u}, nil
}

// Routes mounts the admin routes.
func (a *Admin) Routes(r chi.Router) {
	r.Get("/deploy/{env}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		env, err := envParam(r)
		if err != nil {
			return err
		}
		s, ok, err := Load(r.Context(), a.Pool, env)
		if err != nil {
			return err
		}
		if !ok {
			httpx.JSON(w, 200, map[string]any{"environment": env, "configured": false})
			return nil
		}
		httpx.JSON(w, 200, map[string]any{"environment": env, "configured": true, "settings": s.Settings, "activeSecrets": s.Secrets, "updatedAt": s.UpdatedAt})
		return nil
	}))
	r.Put("/deploy/{env}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		env, err := envParam(r)
		if err != nil {
			return err
		}
		var in Input
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		s, secret, err := a.Put(r.Context(), p, env, in)
		if err != nil {
			return err
		}
		out := map[string]any{"environment": env, "configured": true, "settings": s.Settings, "activeSecrets": s.Secrets}
		if secret != "" {
			out["secret"] = secret // shown once
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
	r.Post("/deploy/{env}/secret/rotate", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		env, err := envParam(r)
		if err != nil {
			return err
		}
		v, err := a.Rotate(r.Context(), p, env)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, map[string]string{"secret": v})
		return nil
	}))
	r.Post("/deploy/{env}/test", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		env, err := envParam(r)
		if err != nil {
			return err
		}
		var in struct {
			Service string `json:"service"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		out, err := a.Test(r.Context(), p, env, in.Service)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
	r.Put("/services/{service}/deploy-override", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		var in struct {
			Env      string            `json:"env"`
			Workflow string            `json:"workflow"`
			Ref      string            `json:"ref"`
			Params   map[string]string `json:"params"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if in.Env != "stage" && in.Env != "production" {
			return apperr.Unprocessable("invalid_env", "env must be stage or production")
		}
		cd := cycledata.New(a.Pool)
		svc, err := cd.ServiceByKey(r.Context(), chi.URLParam(r, "service"))
		if errors.Is(err, cycledata.ErrNotFound) {
			return apperr.NotFound("service_not_found", "service not found")
		}
		if err != nil {
			return err
		}
		if svc.OverrideFromCatalog {
			return apperr.Conflict("catalog_managed", "the override comes from the hammurapi/deploy-workflow annotation of catalog-info.yaml")
		}
		all := map[string]*cicd.Override{}
		_ = json.Unmarshal(svc.DeployOverride, &all)
		if in.Workflow == "" && in.Ref == "" && len(in.Params) == 0 {
			delete(all, in.Env)
		} else {
			all[in.Env] = &cicd.Override{Workflow: in.Workflow, Ref: in.Ref, Params: in.Params}
		}
		raw, _ := json.Marshal(all)
		if err := cd.SetDeployOverride(r.Context(), svc.ID, raw); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
}
