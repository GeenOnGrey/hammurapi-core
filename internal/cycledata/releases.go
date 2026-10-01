package cycledata

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// ─── Releases ────────────────────────────────────────────────────────

// NextReleaseNumber increments the system's release counter.
func (d *DB) NextReleaseNumber(ctx context.Context, systemID uuid.UUID) (int, error) {
	var n int
	err := d.q.QueryRow(ctx, `UPDATE systems SET last_release_number = last_release_number + 1 WHERE id = $1 RETURNING last_release_number`, systemID).Scan(&n)
	return n, nf(err)
}

// InsertRelease creates a release.
func (d *DB) InsertRelease(ctx context.Context, r *Release) error {
	plan, _ := json.Marshal(r.Plan)
	return d.q.QueryRow(ctx, `INSERT INTO releases (key, system_id, number, feature_id, status, plan) VALUES ($1,$2,$3,$4,'merging',$5)
		RETURNING id, created_at`, r.Key, r.SystemID, r.Number, r.FeatureID, plan).Scan(&r.ID, &r.CreatedAt)
}

const releaseCols = `r.id, r.key, r.system_id, r.number, r.feature_id, f.unique_id, f.title, d.key, r.status::text, r.plan, r.merge_started_by,
	r.metric_result, cu.display_name, r.confirmed_at, r.rollback_reason, ru.display_name, r.rolled_back_at, r.blocked_reason, r.created_at`

const releaseFrom = ` FROM releases r JOIN features f ON f.id = r.feature_id JOIN systems s ON s.id = r.system_id JOIN domains d ON d.id = s.domain_id
	LEFT JOIN users cu ON cu.id = r.confirmed_by LEFT JOIN users ru ON ru.id = r.rolled_back_by`

func scanRelease(row interface{ Scan(...any) error }) (*Release, error) {
	var r Release
	var plan []byte
	err := row.Scan(&r.ID, &r.Key, &r.SystemID, &r.Number, &r.FeatureID, &r.Feature, &r.FeatureTitle, &r.Domain, &r.Status, &plan,
		&r.MergeStartedBy, &r.MetricResult, &r.ConfirmedBy, &r.ConfirmedAt, &r.RollbackReason, &r.RolledBackBy, &r.RolledBackAt,
		&r.BlockedReason, &r.CreatedAt)
	if err == nil {
		_ = json.Unmarshal(plan, &r.Plan)
		if r.Plan.Order == nil {
			r.Plan.Order = []string{}
		}
	}
	return &r, err
}

// ReleaseByKey loads a release.
func (d *DB) ReleaseByKey(ctx context.Context, key string) (*Release, error) {
	r, err := scanRelease(d.q.QueryRow(ctx, `SELECT `+releaseCols+releaseFrom+` WHERE r.key = $1`, key))
	return r, nf(err)
}

// ReleaseByID loads a release.
func (d *DB) ReleaseByID(ctx context.Context, id uuid.UUID) (*Release, error) {
	r, err := scanRelease(d.q.QueryRow(ctx, `SELECT `+releaseCols+releaseFrom+` WHERE r.id = $1`, id))
	return r, nf(err)
}

// ReleaseByFeature finds the release of a feature (nil if none).
func (d *DB) ReleaseByFeature(ctx context.Context, featureID uuid.UUID) (*Release, error) {
	r, err := scanRelease(d.q.QueryRow(ctx, `SELECT `+releaseCols+releaseFrom+` WHERE r.feature_id = $1`, featureID))
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return r, err
}

// SetReleaseStatus updates the status and optional blocked reason.
func (d *DB) SetReleaseStatus(ctx context.Context, id uuid.UUID, status string, blocked *string) error {
	_, err := d.q.Exec(ctx, `UPDATE releases SET status = $2::release_status, blocked_reason = $3 WHERE id = $1`, id, status, blocked)
	return err
}

// SetReleaseBlocked sets or clears the blocked reason.
func (d *DB) SetReleaseBlocked(ctx context.Context, id uuid.UUID, blocked *string) error {
	_, err := d.q.Exec(ctx, `UPDATE releases SET blocked_reason = $2 WHERE id = $1`, id, blocked)
	return err
}

// SetReleasePlan stores the merge and deploy order.
func (d *DB) SetReleasePlan(ctx context.Context, id uuid.UUID, plan ReleasePlan) error {
	raw, _ := json.Marshal(plan)
	_, err := d.q.Exec(ctx, `UPDATE releases SET plan = $2 WHERE id = $1`, id, raw)
	return err
}

// StartMerge records who started the merge step.
func (d *DB) StartMerge(ctx context.Context, id, by uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE releases SET merge_started_by = $2 WHERE id = $1`, id, by)
	return err
}

// ConfirmRelease marks a release succeeded.
func (d *DB) ConfirmRelease(ctx context.Context, id, by uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE releases SET status = 'succeeded', confirmed_by = $2, confirmed_at = now(), blocked_reason = NULL WHERE id = $1`, id, by)
	return err
}

// StartRollback marks a release rolling back.
func (d *DB) StartRollback(ctx context.Context, id, by uuid.UUID, reason string) error {
	_, err := d.q.Exec(ctx, `UPDATE releases SET status = 'rolling_back', rollback_reason = $3, rolled_back_by = $2, blocked_reason = NULL WHERE id = $1`, id, by, reason)
	return err
}

// FinishRollback marks a release rolled back.
func (d *DB) FinishRollback(ctx context.Context, id uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE releases SET status = 'rolled_back', rolled_back_at = now(), blocked_reason = NULL WHERE id = $1`, id)
	return err
}

// AddReleasePR links a PR to a release at a position.
func (d *DB) AddReleasePR(ctx context.Context, releaseID, prID uuid.UUID, position int) error {
	_, err := d.q.Exec(ctx, `INSERT INTO release_prs (release_id, pr_id, position) VALUES ($1,$2,$3)
		ON CONFLICT (release_id, pr_id) DO UPDATE SET position = EXCLUDED.position`, releaseID, prID, position)
	return err
}

// ReleasePRs lists PRs of a release in merge order (spec last).
func (d *DB) ReleasePRs(ctx context.Context, releaseID uuid.UUID) ([]PR, error) {
	rows, err := d.q.Query(ctx, `SELECT `+prCols+prFrom+` JOIN release_prs rp ON rp.pr_id = p.id WHERE rp.release_id = $1 ORDER BY rp.position`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PR{}
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ReleaseFilter filters the Release stage list.
type ReleaseFilter struct {
	UserID uuid.UUID
	Domain string
	Status string // active | succeeded | rolled_back | all
	Page   httpx.Page
}

// ListReleases lists releases, newest first.
func (d *DB) ListReleases(ctx context.Context, f ReleaseFilter) ([]Release, error) {
	where := []string{"true"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	switch f.Domain {
	case "", "all":
	case "mine":
		where = append(where, "d.id IN (SELECT domain_id FROM user_domains WHERE user_id = "+arg(f.UserID)+")")
	default:
		where = append(where, "d.key = "+arg(f.Domain))
	}
	switch f.Status {
	case "", "active":
		where = append(where, "r.status NOT IN ('succeeded','rolled_back')")
	case "succeeded":
		where = append(where, "r.status = 'succeeded'")
	case "rolled_back":
		where = append(where, "r.status = 'rolled_back'")
	}
	if c := f.Page.Cursor; c != nil {
		where = append(where, "(r.created_at, r.id::text) < ("+arg(c.T)+", "+arg(c.ID)+")")
	}
	limit := f.Page.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.q.Query(ctx, `SELECT `+releaseCols+releaseFrom+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY r.created_at DESC, r.id::text DESC LIMIT `+arg(limit+1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ─── Deploy runs ─────────────────────────────────────────────────────

const deployCols = `r.id, r.environment::text, r.service_id, s.key, r.feature_id, r.release_id, r.ref, r.status::text, r.signal::text,
	r.version, r.run_url, r.error, u.display_name, r.is_rollback, r.created_at, r.finished_at`

const deployFrom = ` FROM deploy_runs r JOIN services s ON s.id = r.service_id LEFT JOIN users u ON u.id = r.marked_by`

func scanDeploy(row interface{ Scan(...any) error }) (*DeployRun, error) {
	var r DeployRun
	err := row.Scan(&r.ID, &r.Environment, &r.ServiceID, &r.Service, &r.FeatureID, &r.ReleaseID, &r.Ref, &r.Status, &r.Signal,
		&r.Version, &r.RunURL, &r.Error, &r.MarkedBy, &r.IsRollback, &r.CreatedAt, &r.FinishedAt)
	return &r, err
}

// InsertDeployRun creates a deploy run.
func (d *DB) InsertDeployRun(ctx context.Context, r *DeployRun) error {
	return d.q.QueryRow(ctx, `INSERT INTO deploy_runs (id, environment, service_id, feature_id, release_id, ref, status, is_rollback)
		VALUES (COALESCE($1, gen_random_uuid()), $2::deploy_env, $3, $4, $5, $6, 'triggered', $7) RETURNING id, created_at`,
		nilUUID(r.ID), r.Environment, r.ServiceID, r.FeatureID, r.ReleaseID, r.Ref, r.IsRollback).Scan(&r.ID, &r.CreatedAt)
}

// DeployRunByID loads a deploy run.
func (d *DB) DeployRunByID(ctx context.Context, id uuid.UUID) (*DeployRun, error) {
	r, err := scanDeploy(d.q.QueryRow(ctx, `SELECT `+deployCols+deployFrom+` WHERE r.id = $1`, id))
	return r, nf(err)
}

// UpdateDeployRun records a status change. Terminal statuses are final: the
// first signal wins (REL-09).
func (d *DB) UpdateDeployRun(ctx context.Context, id uuid.UUID, status, signal string, version, runURL, errText *string, markedBy *uuid.UUID) (bool, error) {
	tag, err := d.q.Exec(ctx, `UPDATE deploy_runs SET status = $2::deploy_status, signal = COALESCE(NULLIF($3,'')::deploy_signal, signal),
		version = COALESCE($4, version), run_url = COALESCE($5, run_url), error = $6, marked_by = COALESCE($7, marked_by),
		finished_at = CASE WHEN $2 IN ('success','failure','timeout') THEN now() ELSE finished_at END
		WHERE id = $1 AND status NOT IN ('success','failure','timeout')`, id, status, signal, version, runURL, errText, markedBy)
	return tag.RowsAffected() > 0, err
}

// SetDeployRunURL stores the pipeline link after triggering.
func (d *DB) SetDeployRunURL(ctx context.Context, id uuid.UUID, url string) error {
	_, err := d.q.Exec(ctx, `UPDATE deploy_runs SET run_url = $2 WHERE id = $1 AND run_url IS NULL`, id, url)
	return err
}

// ReleaseDeploys lists deploy runs of a release, oldest first.
func (d *DB) ReleaseDeploys(ctx context.Context, releaseID uuid.UUID) ([]DeployRun, error) {
	return d.deploys(ctx, `r.release_id = $1`, releaseID)
}

// StageDeploys lists stage deploys of a feature, oldest first.
func (d *DB) StageDeploys(ctx context.Context, featureID uuid.UUID) ([]DeployRun, error) {
	return d.deploys(ctx, `r.feature_id = $1 AND r.environment = 'stage'`, featureID)
}

// OpenReleaseDeploy finds the latest non-terminal production deploy of a service in a release.
func (d *DB) OpenReleaseDeploy(ctx context.Context, releaseID, serviceID uuid.UUID) (*DeployRun, error) {
	r, err := scanDeploy(d.q.QueryRow(ctx, `SELECT `+deployCols+deployFrom+` WHERE r.release_id = $1 AND r.service_id = $2
		AND r.environment = 'production' ORDER BY r.created_at DESC LIMIT 1`, releaseID, serviceID))
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return r, err
}

func (d *DB) deploys(ctx context.Context, where string, args ...any) ([]DeployRun, error) {
	rows, err := d.q.Query(ctx, `SELECT `+deployCols+deployFrom+` WHERE `+where+` ORDER BY r.created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployRun{}
	for rows.Next() {
		r, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ─── CI results ──────────────────────────────────────────────────────

// TestResult is one test case result.
type TestResult struct {
	TCID       string `json:"testCase"`
	Status     string `json:"status"`
	DurationMS *int   `json:"durationMs"`
}

// InsertCIRun stores a CI run with its results.
func (d *DB) InsertCIRun(ctx context.Context, repo, sha, branch, env, pipelineURL string, results []TestResult) (uuid.UUID, error) {
	var id uuid.UUID
	if err := d.q.QueryRow(ctx, `INSERT INTO ci_runs (repo, sha, branch, environment, pipeline_url) VALUES ($1,$2,NULLIF($3,''),$4,NULLIF($5,'')) RETURNING id`,
		repo, sha, branch, env, pipelineURL).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	for _, r := range results {
		if _, err := d.q.Exec(ctx, `INSERT INTO test_results (ci_run_id, tc_id, status, duration_ms) VALUES ($1,$2,$3,$4)
			ON CONFLICT (ci_run_id, tc_id) DO UPDATE SET status = CASE WHEN test_results.status = 'failed' THEN 'failed' ELSE EXCLUDED.status END`,
			id, r.TCID, r.Status, r.DurationMS); err != nil {
			return uuid.Nil, err
		}
	}
	return id, nil
}

// LatestResults returns the latest result per test case for commits of a repo
// in an environment (ci: PR heads, stage: e2e).
func (d *DB) LatestResults(ctx context.Context, repo string, shas []string, env string) (map[string]TestResult, bool, error) {
	rows, err := d.q.Query(ctx, `SELECT DISTINCT ON (t.tc_id) t.tc_id, t.status, t.duration_ms
		FROM test_results t JOIN ci_runs c ON c.id = t.ci_run_id
		WHERE lower(c.repo) = lower($1) AND c.sha = ANY($2) AND c.environment = $3
		ORDER BY t.tc_id, c.received_at DESC`, repo, shas, env)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := map[string]TestResult{}
	for rows.Next() {
		var r TestResult
		if err := rows.Scan(&r.TCID, &r.Status, &r.DurationMS); err != nil {
			return nil, false, err
		}
		out[r.TCID] = r
	}
	var runs int
	if err := d.q.QueryRow(ctx, `SELECT count(*) FROM ci_runs WHERE lower(repo) = lower($1) AND sha = ANY($2) AND environment = $3`, repo, shas, env).Scan(&runs); err != nil {
		return nil, false, err
	}
	return out, runs > 0, rows.Err()
}

// StageResults returns e2e results from the stage environment reported for a feature's branches.
func (d *DB) StageResults(ctx context.Context, repos []string, shas []string) (map[string]TestResult, bool, error) {
	out := map[string]TestResult{}
	any := false
	for _, repo := range repos {
		rs, ok, err := d.LatestResults(ctx, repo, shas, "stage")
		if err != nil {
			return nil, false, err
		}
		any = any || ok
		for k, v := range rs {
			out[k] = v
		}
	}
	return out, any, nil
}

// ─── Validation ──────────────────────────────────────────────────────

// Signatures lists validation signatures.
func (d *DB) Signatures(ctx context.Context, featureID uuid.UUID) ([]Signature, error) {
	rows, err := d.q.Query(ctx, `SELECT v.side::text, u.display_name, v.user_id, v.comment, v.signed_at FROM validation_signatures v
		JOIN users u ON u.id = v.user_id WHERE v.feature_id = $1 ORDER BY v.side`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Signature{}
	for rows.Next() {
		var s Signature
		if err := rows.Scan(&s.Side, &s.User, &s.UserID, &s.Comment, &s.SignedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Sign adds a signature (idempotent per side).
func (d *DB) Sign(ctx context.Context, featureID uuid.UUID, side string, userID uuid.UUID, comment *string) error {
	_, err := d.q.Exec(ctx, `INSERT INTO validation_signatures (feature_id, side, user_id, comment) VALUES ($1,$2::expert_kind,$3,$4)
		ON CONFLICT (feature_id, side) DO NOTHING`, featureID, side, userID, comment)
	return err
}

// ClearSignatures removes signatures (on return).
func (d *DB) ClearSignatures(ctx context.Context, featureID uuid.UUID) error {
	_, err := d.q.Exec(ctx, `DELETE FROM validation_signatures WHERE feature_id = $1`, featureID)
	return err
}

// AddReturn records a return to rework.
func (d *DB) AddReturn(ctx context.Context, featureID uuid.UUID, target, comment string, userID uuid.UUID) error {
	_, err := d.q.Exec(ctx, `INSERT INTO validation_returns (feature_id, target, comment, user_id) VALUES ($1,$2,$3,$4)`, featureID, target, comment, userID)
	return err
}

// AddDiscrepancy stores a discrepancy found by the agent.
func (d *DB) AddDiscrepancy(ctx context.Context, featureID uuid.UUID, reqID string, prID *uuid.UUID, description string) error {
	_, err := d.q.Exec(ctx, `INSERT INTO discrepancies (feature_id, req_id, pr_id, description) VALUES ($1,$2,$3,$4)`, featureID, reqID, prID, description)
	return err
}

// ResolveDiscrepancies closes open discrepancies (a new check replaces them).
func (d *DB) ResolveDiscrepancies(ctx context.Context, featureID uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE discrepancies SET resolved_at = now() WHERE feature_id = $1 AND resolved_at IS NULL`, featureID)
	return err
}

// OpenDiscrepancies lists unresolved discrepancies.
func (d *DB) OpenDiscrepancies(ctx context.Context, featureID uuid.UUID) ([]Discrepancy, error) {
	rows, err := d.q.Query(ctx, `SELECT x.id, x.req_id, s.key, p.url, x.description, x.found_at, x.resolved_at
		FROM discrepancies x LEFT JOIN pull_requests p ON p.id = x.pr_id LEFT JOIN services s ON s.id = p.service_id
		WHERE x.feature_id = $1 AND x.resolved_at IS NULL ORDER BY x.found_at`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Discrepancy{}
	for rows.Next() {
		var x Discrepancy
		if err := rows.Scan(&x.ID, &x.ReqID, &x.Service, &x.PRURL, &x.Description, &x.FoundAt, &x.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ─── Activity ────────────────────────────────────────────────────────

// AddActivity records a history entry.
func (d *DB) AddActivity(ctx context.Context, subjectType string, subjectID uuid.UUID, typ string, actor *uuid.UUID, isAgent bool, payload any) error {
	raw, _ := json.Marshal(payload)
	if payload == nil {
		raw = []byte("{}")
	}
	_, err := d.q.Exec(ctx, `INSERT INTO activity (subject_type, subject_id, type, actor_id, is_agent, payload) VALUES ($1,$2,$3,$4,$5,$6)`,
		subjectType, subjectID, typ, actor, isAgent, raw)
	return err
}

// ActivityOf lists history entries, newest first.
func (d *DB) ActivityOf(ctx context.Context, subjectType string, subjectID uuid.UUID) ([]Activity, error) {
	rows, err := d.q.Query(ctx, `SELECT a.id, a.type, u.display_name, a.is_agent, a.payload, a.created_at FROM activity a
		LEFT JOIN users u ON u.id = a.actor_id WHERE a.subject_type = $1 AND a.subject_id = $2 ORDER BY a.created_at DESC LIMIT 200`, subjectType, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Type, &a.Actor, &a.IsAgent, &a.Payload, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ─── Settings, flags, metric sources, usage ──────────────────────────

// Setting reads an admin setting into v; false if missing.
func (d *DB) Setting(ctx context.Context, key string, v any) (bool, error) {
	var raw []byte
	err := d.q.QueryRow(ctx, `SELECT value FROM admin_settings WHERE key = $1`, key).Scan(&raw)
	if postgres.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// PutSetting writes an admin setting.
func (d *DB) PutSetting(ctx context.Context, key string, v any, by *uuid.UUID) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.q.Exec(ctx, `INSERT INTO admin_settings (key, value, updated_by, updated_at) VALUES ($1,$2,$3,now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()`, key, raw, by)
	return err
}

// FlagsSetting is admin_settings.feature_flags.
type FlagsSetting struct {
	Enabled    bool     `json:"enabled"`
	SecretRefs []string `json:"secretRefs"`
}

// StageSetting is admin_settings.stage.
type StageSetting struct {
	Enabled bool `json:"enabled"`
}

// CatalogSetting is admin_settings.catalog.
type CatalogSetting struct {
	Enabled         bool       `json:"enabled"`
	CatalogRepo     string     `json:"catalogRepo"`
	CatalogGlob     string     `json:"catalogGlob"`
	ServiceFilePath string     `json:"serviceFilePath"`
	ServiceRepos    []string   `json:"serviceRepos"`
	LastSyncAt      *time.Time `json:"lastSyncAt"`
	LastSyncError   *string    `json:"lastSyncError"`
}

// InsertFlagEvent stores a flag change.
func (d *DB) InsertFlagEvent(ctx context.Context, flag, state, env string, at time.Time, actor, source string) error {
	_, err := d.q.Exec(ctx, `INSERT INTO flag_events (flag_key, state, environment, changed_at, actor, source) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6)`,
		flag, state, env, at, actor, source)
	return err
}

// LastFlagState returns the last production state of a flag ("" if none).
func (d *DB) LastFlagState(ctx context.Context, flag string) (string, *time.Time, error) {
	var st string
	var at time.Time
	err := d.q.QueryRow(ctx, `SELECT state, changed_at FROM flag_events WHERE flag_key = $1 AND environment = 'production'
		ORDER BY changed_at DESC, received_at DESC LIMIT 1`, flag).Scan(&st, &at)
	if postgres.IsNoRows(err) {
		return "", nil, nil
	}
	return st, &at, err
}

// MetricSource is a configured read-only metric source.
type MetricSource struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"` // clickhouse | prometheus
	Endpoint  string          `json:"endpoint"`
	Username  *string         `json:"username"`
	SecretRef string          `json:"secretRef"`
	Limits    json.RawMessage `json:"limits"`
}

// MetricSources lists sources.
func (d *DB) MetricSources(ctx context.Context) ([]MetricSource, error) {
	rows, err := d.q.Query(ctx, `SELECT name, type, endpoint, username, secret_ref, limits FROM metric_sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricSource{}
	for rows.Next() {
		var m MetricSource
		if err := rows.Scan(&m.Name, &m.Type, &m.Endpoint, &m.Username, &m.SecretRef, &m.Limits); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MetricSourceByName loads one source.
func (d *DB) MetricSourceByName(ctx context.Context, name string) (*MetricSource, error) {
	var m MetricSource
	err := d.q.QueryRow(ctx, `SELECT name, type, endpoint, username, secret_ref, limits FROM metric_sources WHERE name = $1`, name).
		Scan(&m.Name, &m.Type, &m.Endpoint, &m.Username, &m.SecretRef, &m.Limits)
	return &m, nf(err)
}

// UpsertMetricSource creates or updates a source.
func (d *DB) UpsertMetricSource(ctx context.Context, m MetricSource) error {
	limits := m.Limits
	if len(limits) == 0 {
		limits = json.RawMessage("{}")
	}
	_, err := d.q.Exec(ctx, `INSERT INTO metric_sources (name, type, endpoint, username, secret_ref, limits) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (name) DO UPDATE SET type = EXCLUDED.type, endpoint = EXCLUDED.endpoint, username = EXCLUDED.username,
		secret_ref = CASE WHEN EXCLUDED.secret_ref <> '' THEN EXCLUDED.secret_ref ELSE metric_sources.secret_ref END, limits = EXCLUDED.limits`,
		m.Name, m.Type, m.Endpoint, m.Username, m.SecretRef, limits)
	return err
}

// DeleteMetricSource removes a source.
func (d *DB) DeleteMetricSource(ctx context.Context, name string) error {
	_, err := d.q.Exec(ctx, `DELETE FROM metric_sources WHERE name = $1`, name)
	return err
}

// Usage links agent token usage to cycle entities.
type Usage struct {
	Context   string
	IssueID   *uuid.UUID
	FeatureID *uuid.UUID
	ReleaseID *uuid.UUID
	TaskID    *uuid.UUID
	UserID    *uuid.UUID
	Model     string
	TokensIn  int64
	TokensOut int64
}

// AddUsage records agent token usage.
func (d *DB) AddUsage(ctx context.Context, u Usage) error {
	_, err := d.q.Exec(ctx, `INSERT INTO agent_usage (context, issue_id, feature_id, release_id, task_id, user_id, model, tokens_in, tokens_out)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)`, u.Context, u.IssueID, u.FeatureID, u.ReleaseID, u.TaskID, u.UserID, u.Model, u.TokensIn, u.TokensOut)
	return err
}

// UsageTotals sums tokens for a column (issue_id, feature_id, release_id).
func (d *DB) UsageTotals(ctx context.Context, column string, id uuid.UUID) (in, out int64, err error) {
	switch column {
	case "issue_id", "feature_id", "release_id", "task_id":
	default:
		return 0, 0, fmt.Errorf("bad usage column %s", column)
	}
	err = d.q.QueryRow(ctx, `SELECT COALESCE(sum(tokens_in),0), COALESCE(sum(tokens_out),0) FROM agent_usage WHERE `+column+` = $1`, id).Scan(&in, &out)
	return
}

// UserByUsername resolves a provider login.
func (d *DB) UserByUsername(ctx context.Context, login string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := d.q.QueryRow(ctx, `SELECT id FROM users WHERE lower(username) = lower($1)`, login).Scan(&id)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return &id, err
}

// Username returns a user's login.
func (d *DB) Username(ctx context.Context, id uuid.UUID) (string, error) {
	var s string
	err := d.q.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, id).Scan(&s)
	return s, nf(err)
}
