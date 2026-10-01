// Package releases implements releases (PLT.HMR-0002 R25–R34): creation after
// validation, the release workflow — merge of service PRs in the rollout order,
// production deploys with the first-signal-wins rule, feature flags,
// confirmation with the merge of the specification PR — and the release API.
// The rollback workflow lives in package rollback.
package releases

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

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/deploy"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Create creates the release of a validated feature (R25) in the caller's
// transaction: key RLS.<D>.<S>-NNNN, plan from the rollout order of the arch
// spec, links to every service PR and to the spec PR (last), and the workflow.
func Create(ctx context.Context, tx specdata.Store, f *specdata.Feature, arch string, specRepo string) (*cycledata.Release, error) {
	cd := cycledata.New(tx.Q())
	sys, err := tx.SystemByKeys(ctx, f.DomainKey, f.SystemKey)
	if err != nil {
		return nil, err
	}
	n, err := cd.NextReleaseNumber(ctx, sys.ID)
	if err != nil {
		return nil, err
	}
	prs, err := cd.FeaturePRs(ctx, f.ID, "service")
	if err != nil {
		return nil, err
	}
	var services []string
	byService := map[string]cycledata.PR{}
	for _, pr := range prs {
		if pr.State != "open" || pr.Service == nil {
			continue
		}
		if _, dup := byService[*pr.Service]; dup {
			continue
		}
		byService[*pr.Service] = pr
		services = append(services, *pr.Service)
	}
	order := markdown.ParseRolloutOrder(arch, services)
	rel := &cycledata.Release{Key: domain.ReleaseKey(f.DomainKey, f.SystemKey, n), SystemID: sys.ID, Number: n, FeatureID: f.ID,
		Plan: cycledata.ReleasePlan{Order: order}}
	if err := cd.InsertRelease(ctx, rel); err != nil {
		if postgres.IsUniqueViolation(err) {
			return nil, apperr.Conflict("release_exists", "the feature already has a release") // WF-05
		}
		return nil, err
	}
	for i, svc := range order {
		if err := cd.AddReleasePR(ctx, rel.ID, byService[svc].ID, i); err != nil {
			return nil, err
		}
	}
	spec := &cycledata.PR{Repo: specRepo, Number: f.PRNumber, URL: f.PRURL, Title: f.UniqueID + " " + f.Title, Branch: f.Branch, Kind: "spec",
		FeatureID: f.ID, State: "open", Review: "not_required"}
	if err := cd.UpsertPR(ctx, spec); err != nil {
		return nil, err
	}
	if err := cd.AddReleasePR(ctx, rel.ID, spec.ID, len(order)); err != nil {
		return nil, err
	}
	if _, err := workflows.Start(ctx, tx.Q(), Kind, rel.ID, nil, "merging", map[string]any{"order": order}); err != nil {
		return nil, err
	}
	if _, err := tx.Q().Exec(ctx, `UPDATE workflow_runs SET step = $2 WHERE kind = 'release' AND subject_id = $1`, rel.ID, stepAwaitingStart); err != nil {
		return nil, err
	}
	if err := cd.AddActivity(ctx, "release", rel.ID, "created", nil, false, map[string]any{"feature": f.UniqueID, "order": order}); err != nil {
		return nil, err
	}
	return rel, nil
}

// Service implements the release API.
type Service struct {
	pool   *pgxpool.Pool
	store  specdata.Store
	events events.Publisher
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, store specdata.Store, ev events.Publisher) *Service {
	return &Service{pool: pool, store: store, events: ev}
}

// Errors.
var (
	ErrNotFound    = apperr.NotFound("release_not_found", "release not found")
	errFinished    = apperr.Conflict("release_finished", "the release is finished")
	errStepInvalid = apperr.Conflict("release_step_invalid", "the action does not match the current step of the release")
)

func (s *Service) load(ctx context.Context, key string) (*cycledata.Release, *specdata.Feature, *workflows.Run, error) {
	cd := cycledata.New(s.pool)
	rel, err := cd.ReleaseByKey(ctx, key)
	if errors.Is(err, cycledata.ErrNotFound) {
		return nil, nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, nil, err
	}
	f, err := s.store.FeatureByID(ctx, rel.FeatureID)
	if err != nil {
		return nil, nil, nil, err
	}
	run, err := workflows.LatestRun(ctx, s.pool, Kind, rel.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	return rel, f, run, nil
}

func (s *Service) action(ctx context.Context, p *domain.Principal, key string, technical bool) (*cycledata.Release, *specdata.Feature, *workflows.Run, error) {
	rel, f, run, err := s.load(ctx, key)
	if err != nil {
		return nil, nil, nil, err
	}
	if technical {
		if !p.HasExpert(f.DomainKey, domain.ExpertTechnical) {
			return nil, nil, nil, apperr.Forbidden("forbidden", "technical expert of domain "+f.DomainKey+" required")
		}
	} else if !p.IsExpertOf(f.DomainKey) {
		return nil, nil, nil, apperr.Forbidden("forbidden", "expert of domain "+f.DomainKey+" required")
	}
	if rel.Status == "succeeded" || rel.Status == "rolled_back" {
		return nil, nil, nil, errFinished // REL-19
	}
	return rel, f, run, nil
}

// Card is GET /releases/{key}.
type Card struct {
	*cycledata.Release
	Issues      []string              `json:"issues"`
	PRs         []cycledata.PR        `json:"prs"`
	Deploys     []cycledata.DeployRun `json:"deploys"`
	Step        string                `json:"step"`
	Blocked     bool                  `json:"blocked"`
	Current     *string               `json:"currentService"`
	FlagKey     *string               `json:"flagKey"`
	FlagState   *string               `json:"flagState"`
	FlagsOn     bool                  `json:"flagsEnabled"`
	Metric      any                   `json:"metric"`
	Rollback    *RollbackView         `json:"rollback"`
	Activity    []cycledata.Activity  `json:"activity"`
	Tasks       []cycledata.Task      `json:"tasks"`
	TokensIn    int64                 `json:"tokensIn"`
	TokensOut   int64                 `json:"tokensOut"`
	Permissions Permissions           `json:"permissions"`
}

// RollbackView is the state of the rollback workflow.
type RollbackView struct {
	State     string  `json:"state"`
	Step      string  `json:"step"`
	LastError *string `json:"lastError"`
}

// Permissions of the release card.
type Permissions struct {
	EditPlan   bool `json:"editPlan"`
	StartMerge bool `json:"startMerge"`
	MarkDeploy bool `json:"markDeploy"`
	Retry      bool `json:"retry"`
	MarkFlag   bool `json:"markFlag"`
	Confirm    bool `json:"confirm"`
	Rollback   bool `json:"rollback"`
}

// Get builds the release card (REL-01).
func (s *Service) Get(ctx context.Context, p *domain.Principal, key string) (*Card, error) {
	rel, f, run, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	cd := cycledata.New(s.pool)
	c := &Card{Release: rel, FlagKey: f.FlagKey, Metric: f.Metric}
	if c.Issues, err = s.store.FeatureIssueKeys(ctx, f.ID); err != nil {
		return nil, err
	}
	if c.PRs, err = cd.ReleasePRs(ctx, rel.ID); err != nil {
		return nil, err
	}
	if c.Deploys, err = cd.ReleaseDeploys(ctx, rel.ID); err != nil {
		return nil, err
	}
	if c.Activity, err = cd.ActivityOf(ctx, "release", rel.ID); err != nil {
		return nil, err
	}
	if c.Tasks, err = cd.ReleaseTasks(ctx, rel.ID); err != nil {
		return nil, err
	}
	for i := range c.Tasks {
		c.Tasks[i].Input = nil
	}
	if c.TokensIn, c.TokensOut, err = cd.UsageTotals(ctx, "release_id", rel.ID); err != nil {
		return nil, err
	}
	if c.Issues == nil {
		c.Issues = []string{}
	}
	if c.Activity == nil {
		c.Activity = []cycledata.Activity{}
	}
	if f.FlagKey != nil {
		if st, _, err := cd.LastFlagState(ctx, *f.FlagKey); err == nil && st != "" {
			c.FlagState = &st
		}
	}
	var fs cycledata.FlagsSetting
	if _, err := cd.Setting(ctx, "feature_flags", &fs); err == nil {
		c.FlagsOn = fs.Enabled
	}
	if run != nil {
		c.Step = run.Step
		c.Blocked = run.State == workflows.StateBlocked
		v := load(run.Context)
		if v.Idx < len(v.Order) && (strings.HasPrefix(run.Step, "merge") || strings.HasPrefix(run.Step, "deploy") || run.Step == stepUpdateWait) {
			svc := v.Order[v.Idx]
			c.Current = &svc
		}
	}
	if rb, err := workflows.LatestRun(ctx, s.pool, RollbackKind, rel.ID); err == nil && rb != nil {
		c.Rollback = &RollbackView{State: rb.State, Step: rb.Step, LastError: rb.LastError}
	}
	expert := p.IsExpertOf(f.DomainKey)
	finished := rel.Status == "succeeded" || rel.Status == "rolled_back" || rel.Status == "rolling_back"
	if expert && !finished && run != nil {
		c.Permissions.EditPlan = run.Step == stepAwaitingStart && run.State != workflows.StateBlocked
		c.Permissions.StartMerge = c.Permissions.EditPlan
		c.Permissions.Retry = c.Blocked
		c.Permissions.MarkDeploy = p.HasExpert(f.DomainKey, domain.ExpertTechnical) && (run.Step == stepDeployWait || (c.Blocked && run.Step == stepDeployWait))
		c.Permissions.MarkFlag = run.Step == stepFlagsWait
		c.Permissions.Confirm = run.Step == stepAwaitConfirm && !c.Blocked
		c.Permissions.Rollback = true
	}
	if rel.Status == "rolling_back" && expert && c.Rollback != nil {
		c.Permissions.Retry = c.Rollback.State == workflows.StateBlocked
		c.Permissions.MarkDeploy = p.HasExpert(f.DomainKey, domain.ExpertTechnical) && c.Rollback.Step == "deploy_wait"
		c.Permissions.MarkFlag = c.Rollback.Step == "flags_wait"
	}
	return c, nil
}

// SetPlan changes the rollout order before the merge starts (REL-02).
func (s *Service) SetPlan(ctx context.Context, p *domain.Principal, key string, order []string) error {
	rel, _, run, err := s.action(ctx, p, key, false)
	if err != nil {
		return err
	}
	if run == nil || run.Step != stepAwaitingStart || run.State == workflows.StateBlocked {
		return errStepInvalid
	}
	have := map[string]bool{}
	for _, x := range rel.Plan.Order {
		have[x] = true
	}
	if len(order) != len(rel.Plan.Order) {
		return apperr.Unprocessable("invalid_plan", "the plan must list every service of the release exactly once")
	}
	for _, x := range order {
		if !have[x] {
			return apperr.Unprocessable("invalid_plan", "unknown service "+x)
		}
		delete(have, x)
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cd := cycledata.New(tx)
		if err := cd.SetReleasePlan(ctx, rel.ID, cycledata.ReleasePlan{Order: order}); err != nil {
			return err
		}
		prs, err := cd.ReleasePRs(ctx, rel.ID)
		if err != nil {
			return err
		}
		for i, svc := range order {
			if pr := ServicePR(prs, svc); pr != nil {
				if err := cd.AddReleasePR(ctx, rel.ID, pr.ID, i); err != nil {
					return err
				}
			}
		}
		c := run.Context
		c["order"] = order
		_, err = tx.Exec(ctx, `UPDATE workflow_runs SET context = context || jsonb_build_object('order', $2::jsonb), version = version + 1 WHERE id = $1`,
			run.ID, mustJSON(order))
		return err
	})
}

func mustJSON(v any) string {
	b, _ := jsonMarshal(v)
	return string(b)
}

func (s *Service) send(ctx context.Context, run *workflows.Run, typ string, payload any) error {
	if err := workflows.Send(ctx, s.pool, run.ID, typ, payload); err != nil {
		return err
	}
	s.events.Publish(ctx, events.Event{Type: events.FocusChanged, Data: map[string]any{"run": run.ID}})
	return nil
}

// StartMerge is POST /releases/{key}/merge (R27).
func (s *Service) StartMerge(ctx context.Context, p *domain.Principal, key string) error {
	_, _, run, err := s.action(ctx, p, key, false)
	if err != nil {
		return err
	}
	if run == nil || run.Step != stepAwaitingStart || run.State == workflows.StateBlocked {
		return errStepInvalid
	}
	return s.send(ctx, run, "merge_started", map[string]any{"userId": p.UserID})
}

// Retry resumes a blocked release or rollback (deploy failure, merge refusal, timeout).
func (s *Service) Retry(ctx context.Context, p *domain.Principal, key string) error {
	rel, _, run, err := s.action(ctx, p, key, false)
	if err != nil {
		return err
	}
	if rel.Status == "rolling_back" {
		rb, err := workflows.LatestRun(ctx, s.pool, RollbackKind, rel.ID)
		if err != nil {
			return err
		}
		if rb == nil || rb.State != workflows.StateBlocked {
			return errStepInvalid
		}
		return s.send(ctx, rb, "retry", map[string]any{"userId": p.UserID})
	}
	if run == nil || run.State != workflows.StateBlocked {
		return errStepInvalid
	}
	return s.send(ctx, run, "retry", map[string]any{"userId": p.UserID})
}

// MarkDeploy is the manual mark "released" of a service (R28, REL-10).
func (s *Service) MarkDeploy(ctx context.Context, p *domain.Principal, key, service, version string) error {
	rel, _, _, err := s.action(ctx, p, key, true)
	if err != nil {
		return err
	}
	cd := cycledata.New(s.pool)
	svc, err := cd.ServiceByKey(ctx, service)
	if errors.Is(err, cycledata.ErrNotFound) {
		return apperr.NotFound("service_not_found", "service not found")
	}
	if err != nil {
		return err
	}
	run, err := cd.OpenReleaseDeploy(ctx, rel.ID, svc.ID)
	if err != nil {
		return err
	}
	if run == nil || run.Status == "success" {
		return errStepInvalid
	}
	if run.Status == "failure" || run.Status == "timeout" {
		// A person confirms the service works although the pipeline failed: a new manual record.
		nr := &cycledata.DeployRun{Environment: "production", ServiceID: svc.ID, ReleaseID: &rel.ID, Ref: run.Ref, IsRollback: run.IsRollback}
		if err := cd.InsertDeployRun(ctx, nr); err != nil {
			return err
		}
		run = nr
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := deploy.Mark(ctx, tx, run.ID, version, p.UserID); err != nil {
			return err
		}
		// A mark after a failure also resumes the blocked workflow.
		for _, kind := range []string{Kind, RollbackKind} {
			var id uuid.UUID
			err := tx.QueryRow(ctx, `SELECT id FROM workflow_runs WHERE kind = $1 AND subject_id = $2 AND state = 'blocked'`, kind, rel.ID).Scan(&id)
			if err == nil {
				if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET context = context || jsonb_build_object('deployRunId', $2::text), version = version + 1 WHERE id = $1`,
					id, run.ID.String()); err != nil {
					return err
				}
				if err := workflows.Send(ctx, tx, id, "retry_mark", map[string]any{}); err != nil {
					return err
				}
			}
		}
		return cycledata.New(tx).AddActivity(ctx, "release", rel.ID, "deploy_marked", &p.UserID, false, map[string]any{"service": service, "version": version})
	})
}

// MarkFlag records a manual flag state (R29): only production events count.
func (s *Service) MarkFlag(ctx context.Context, p *domain.Principal, key, state string, at time.Time) error {
	rel, f, _, err := s.action(ctx, p, key, false)
	if err != nil && (!errors.Is(err, errFinished) || rel == nil) {
		return err
	}
	if state != "on" && state != "off" {
		return apperr.Unprocessable("invalid_state", "state must be on or off")
	}
	if f.FlagKey == nil || *f.FlagKey == "" {
		return apperr.Conflict("no_flag", "the feature has no flag key")
	}
	if at.IsZero() {
		at = time.Now()
	}
	login, _ := cycledata.New(s.pool).Username(ctx, p.UserID)
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return RecordFlag(ctx, tx, *f.FlagKey, state, "production", at, login, "manual")
	})
}

// RecordFlag stores a flag event and delivers production changes to the
// releases and rollbacks of features with this flag (REL-11, REL-12).
func RecordFlag(ctx context.Context, tx pgx.Tx, flag, state, env string, at time.Time, actor, source string) error {
	cd := cycledata.New(tx)
	if err := cd.InsertFlagEvent(ctx, flag, state, env, at, actor, source); err != nil {
		return err
	}
	if env != "production" {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT r.id FROM releases r JOIN features f ON f.id = r.feature_id
		WHERE f.flag_key = $1 AND r.status NOT IN ('succeeded','rolled_back')`, flag)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		for _, kind := range []string{Kind, RollbackKind} {
			if _, err := workflows.SendToSubject(ctx, tx, kind, id, "flag_"+state, map[string]any{"flag": flag, "at": at}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Confirm is POST /releases/{key}/confirm (R31).
func (s *Service) Confirm(ctx context.Context, p *domain.Principal, key string) error {
	_, _, run, err := s.action(ctx, p, key, false)
	if err != nil {
		return err
	}
	if run == nil || run.Step != stepAwaitConfirm || run.State == workflows.StateBlocked {
		return errStepInvalid
	}
	return s.send(ctx, run, "confirmed", map[string]any{"userId": p.UserID})
}

// Rollback is POST /releases/{key}/rollback (R32, R33).
func (s *Service) Rollback(ctx context.Context, p *domain.Principal, key, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.Unprocessable("reason_required", "a reason is required") // RB-01
	}
	rel, _, run, err := s.action(ctx, p, key, false)
	if err != nil {
		return err
	}
	if rel.Status == "rolling_back" || run == nil || run.State == "rolling_back" {
		return errStepInvalid
	}
	if run.Step == stepConfirmWait {
		return apperr.Conflict("release_step_invalid", "the release is being confirmed")
	}
	return s.send(ctx, run, "rollback", map[string]any{"userId": p.UserID, "reason": reason})
}

// Metric is GET /releases/{key}/metric: the evaluation window is the next release
// of the feature (R30), so only the target and the source are returned.
func (s *Service) Metric(ctx context.Context, key string) (map[string]any, error) {
	_, f, _, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	return map[string]any{"metric": f.Metric, "samples": []any{}, "pauses": []any{}, "evaluation": "not_available"}, nil
}

// Routes mounts /api/v1/releases.
func (s *Service) Routes(r chi.Router) {
	r.Get("/releases", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		page, err := httpx.ParsePage(r)
		if err != nil {
			return err
		}
		q := r.URL.Query()
		items, err := cycledata.New(s.pool).ListReleases(r.Context(), cycledata.ReleaseFilter{UserID: p.UserID, Domain: q.Get("domain"), Status: q.Get("status"), Page: page})
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, httpx.NewList(items, page.Limit, func(x cycledata.Release) (time.Time, string) { return x.CreatedAt, x.ID.String() }))
		return nil
	}))
	r.Get("/releases/{key}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		c, err := s.Get(r.Context(), p, chi.URLParam(r, "key"))
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, c)
		return nil
	}))
	r.Get("/releases/{key}/metric", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		out, err := s.Metric(r.Context(), chi.URLParam(r, "key"))
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
	post := func(path string, fn func(r *http.Request, p *domain.Principal, key string) error) {
		r.Method(http.MethodPost, path, httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			if err := fn(r, p, chi.URLParam(r, "key")); err != nil {
				return err
			}
			w.WriteHeader(http.StatusAccepted)
			return nil
		}))
	}
	r.Put("/releases/{key}/plan", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			Order []string `json:"order"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.SetPlan(r.Context(), p, chi.URLParam(r, "key"), in.Order); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
	post("/releases/{key}/merge", func(r *http.Request, p *domain.Principal, key string) error { return s.StartMerge(r.Context(), p, key) })
	post("/releases/{key}/retry", func(r *http.Request, p *domain.Principal, key string) error { return s.Retry(r.Context(), p, key) })
	post("/releases/{key}/deploys/{service}/retry", func(r *http.Request, p *domain.Principal, key string) error { return s.Retry(r.Context(), p, key) })
	post("/releases/{key}/deploys/{service}/mark", func(r *http.Request, p *domain.Principal, key string) error {
		var in struct {
			Version    string    `json:"version"`
			ReleasedAt time.Time `json:"releasedAt"`
		}
		if r.ContentLength > 0 {
			if err := httpx.Decode(r, &in); err != nil {
				return err
			}
		}
		return s.MarkDeploy(r.Context(), p, key, chi.URLParam(r, "service"), in.Version)
	})
	post("/releases/{key}/flag/mark", func(r *http.Request, p *domain.Principal, key string) error {
		var in struct {
			State string    `json:"state"`
			At    time.Time `json:"at"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		return s.MarkFlag(r.Context(), p, key, in.State, in.At)
	})
	post("/releases/{key}/confirm", func(r *http.Request, p *domain.Principal, key string) error { return s.Confirm(r.Context(), p, key) })
	post("/releases/{key}/rollback", func(r *http.Request, p *domain.Principal, key string) error {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		return s.Rollback(r.Context(), p, key, in.Reason)
	})
}

// SpecRepoOf returns the repository of the specification PRs.
func SpecRepoOf(p git.Provider) string { return p.Repo() }

var jsonMarshal = json.Marshal
