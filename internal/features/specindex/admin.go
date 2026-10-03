package specindex

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// RunInfo is a check of the repository.
type RunInfo struct {
	ID         uuid.UUID  `json:"id"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	Commit     *string    `json:"commit"`
	StartedAt  *time.Time `json:"startedAt"`
	DurationMs *int64     `json:"durationMs"`
	Found      *int       `json:"found"`
	Indexed    *int       `json:"indexed"`
	Issues     *int       `json:"issues"`
	Error      *string    `json:"error"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// CheckNow is "Check now" (SCN-04, SCN-05): a repeated request returns the
// same queued run.
func (s *Service) CheckNow(ctx context.Context, p *domain.Principal) (uuid.UUID, error) {
	if err := requireGlobal(p); err != nil {
		return uuid.Nil, err
	}
	return s.Enqueue(ctx, s.pool, TriggerManual, &p.UserID)
}

// Runs lists the checks, newest first.
func (s *Service) Runs(ctx context.Context, p *domain.Principal, page httpx.Page) (httpx.List[RunInfo], error) {
	if err := requireGlobal(p); err != nil {
		return httpx.List[RunInfo]{}, err
	}
	args := []any{page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` WHERE (created_at, id::text) < ($2, $3)`
	}
	rows, err := s.pool.Query(ctx, `SELECT id, trigger, status, commit_sha, started_at,
			(EXTRACT(EPOCH FROM (finished_at - started_at)) * 1000)::bigint, found, indexed, issues, error, created_at
		FROM spec_scan_runs`+cond+` ORDER BY created_at DESC, id::text DESC LIMIT $1`, args...)
	if err != nil {
		return httpx.List[RunInfo]{}, err
	}
	defer rows.Close()
	var items []RunInfo
	for rows.Next() {
		var r RunInfo
		if err := rows.Scan(&r.ID, &r.Trigger, &r.Status, &r.Commit, &r.StartedAt, &r.DurationMs, &r.Found, &r.Indexed,
			&r.Issues, &r.Error, &r.CreatedAt); err != nil {
			return httpx.List[RunInfo]{}, err
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return httpx.List[RunInfo]{}, err
	}
	return httpx.NewList(items, page.Limit, func(r RunInfo) (time.Time, string) { return r.CreatedAt, r.ID.String() }), nil
}

// IssueInfo is an indexing problem.
type IssueInfo struct {
	ID          uuid.UUID      `json:"id"`
	Path        string         `json:"path"`
	FeatureKey  *string        `json:"featureKey"`
	Kind        string         `json:"kind"`
	Details     map[string]any `json:"details"`
	FirstSeenAt time.Time      `json:"firstSeenAt"`
	LastSeenAt  time.Time      `json:"lastSeenAt"`
	ResolvedAt  *time.Time     `json:"resolvedAt"`
}

// Issues lists indexing problems: open (default) or all; kind narrows them.
func (s *Service) Issues(ctx context.Context, p *domain.Principal, status, kind string) ([]IssueInfo, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, path, feature_key, kind, details, first_seen_at, last_seen_at, resolved_at
		FROM spec_index_issues WHERE ($1 = 'all' OR resolved_at IS NULL) AND ($2 = '' OR kind = $2)
		ORDER BY resolved_at IS NOT NULL, first_seen_at DESC LIMIT 500`, status, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IssueInfo{}
	for rows.Next() {
		var it IssueInfo
		var raw []byte
		if err := rows.Scan(&it.ID, &it.Path, &it.FeatureKey, &it.Kind, &raw, &it.FirstSeenAt, &it.LastSeenAt, &it.ResolvedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &it.Details)
		out = append(out, it)
	}
	return out, rows.Err()
}

// MissingItem is a domain/system the repository needs (R6).
type MissingItem struct {
	Domain             string   `json:"domain"`
	DomainExists       bool     `json:"domainExists"`
	System             string   `json:"system"`
	Features           []string `json:"features"`
	CatalogInfoExample string   `json:"catalogInfoExample,omitempty"`
}

// MissingCatalog is GET /admin/api/v1/spec-scan/missing-catalog.
type MissingCatalog struct {
	CatalogSource string        `json:"catalogSource"` // manual | backstage
	Items         []MissingItem `json:"items"`
}

// Missing lists the domains and systems that specifications wait for (CAT-01,
// CAT-02); with Backstage as the master of the catalog it gives an example of
// catalog-info.yaml instead of "Add" (CAT-04). Any administrator may read it.
func (s *Service) Missing(ctx context.Context, p *domain.Principal) (*MissingCatalog, error) {
	if err := requireAnyAdmin(p); err != nil {
		return nil, err
	}
	out := &MissingCatalog{CatalogSource: "manual", Items: []MissingItem{}}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT value FROM admin_settings WHERE key = 'catalog'`).Scan(&raw); err == nil {
		var c struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal(raw, &c) == nil && c.Enabled {
			out.CatalogSource = "backstage"
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, details->>'domain', details->>'system', COALESCE(feature_key, path),
			EXISTS (SELECT 1 FROM domains d WHERE d.key = details->>'domain')
		FROM spec_index_issues WHERE resolved_at IS NULL AND kind IN ('missing_domain', 'missing_system')
		ORDER BY 2, 3, 4`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]*MissingItem{}
	var order []string
	for rows.Next() {
		var kind, d, sy, key string
		var exists bool
		if err := rows.Scan(&kind, &d, &sy, &key, &exists); err != nil {
			return nil, err
		}
		k := d + "/" + sy
		it := byKey[k]
		if it == nil {
			it = &MissingItem{Domain: d, System: sy, DomainExists: exists, Features: []string{}}
			byKey[k] = it
			order = append(order, k)
		}
		it.Features = append(it.Features, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(order)
	for _, k := range order {
		it := byKey[k]
		if out.CatalogSource == "backstage" {
			it.CatalogInfoExample = CatalogInfoExample(it.Domain, it.System, !it.DomainExists)
		}
		out.Items = append(out.Items, *it)
	}
	return out, nil
}

// CatalogInfoExample is a catalog-info.yaml fragment for Backstage: entity
// names are the keys in lower case, hammurapi/key carries the key, the owner
// is a placeholder to replace.
func CatalogInfoExample(domainKey, systemKey string, withDomain bool) string {
	var b strings.Builder
	d, sy := strings.ToLower(domainKey), strings.ToLower(systemKey)
	if withDomain {
		fmt.Fprintf(&b, `apiVersion: backstage.io/v1alpha1
kind: Domain
metadata:
  name: %s
  annotations:
    hammurapi/key: %s
spec:
  owner: team-%s
---
`, d, domainKey, d)
	}
	fmt.Fprintf(&b, `apiVersion: backstage.io/v1alpha1
kind: System
metadata:
  name: %s
  annotations:
    hammurapi/key: %s
spec:
  owner: team-%s
  domain: %s
`, sy, systemKey, d, d)
	return b.String()
}
