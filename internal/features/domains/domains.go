// Package domains implements the dictionary of domains and systems and the
// per-domain approval switch.
package domains

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
)

// System is a system in the dictionary.
type System struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	LastNumber int    `json:"lastNumber"`
}

// Domain is a domain with its systems.
type Domain struct {
	ID               uuid.UUID `json:"-"`
	Key              string    `json:"key"`
	Name             string    `json:"name"`
	ApprovalRequired bool      `json:"approvalRequired"`
	CreatedAt        time.Time `json:"createdAt"`
	Systems          []System  `json:"systems"`
}

// Repository persists the dictionary.
type Repository struct{ q postgres.Querier }

// NewRepository creates a repository.
func NewRepository(q postgres.Querier) *Repository { return &Repository{q: q} }

// List returns all domains with systems.
func (r *Repository) List(ctx context.Context) ([]Domain, error) {
	rows, err := r.q.Query(ctx, `SELECT d.id, d.key, d.name, d.approval_required, d.created_at, s.key, s.name, s.last_number
		FROM domains d LEFT JOIN systems s ON s.domain_id = d.id ORDER BY d.key, s.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Domain{}
	idx := map[uuid.UUID]int{}
	for rows.Next() {
		var d Domain
		var sk, sn *string
		var ln *int
		if err := rows.Scan(&d.ID, &d.Key, &d.Name, &d.ApprovalRequired, &d.CreatedAt, &sk, &sn, &ln); err != nil {
			return nil, err
		}
		i, ok := idx[d.ID]
		if !ok {
			d.Systems = []System{}
			out = append(out, d)
			i = len(out) - 1
			idx[d.ID] = i
		}
		if sk != nil {
			out[i].Systems = append(out[i].Systems, System{Key: *sk, Name: *sn, LastNumber: *ln})
		}
	}
	return out, rows.Err()
}

// Service implements dictionary use cases.
type Service struct {
	repo   *Repository
	events events.Publisher
}

// NewService creates the service.
func NewService(repo *Repository, ev events.Publisher) *Service {
	return &Service{repo: repo, events: ev}
}

func requireAnyAdmin(p *domain.Principal) error {
	if !p.IsAnyAdmin() {
		return apperr.Forbidden("forbidden", "administrator role required")
	}
	return nil
}

// CreateDomain adds a domain.
func (s *Service) CreateDomain(ctx context.Context, p *domain.Principal, key, name string, approvalRequired bool) error {
	if err := requireAnyAdmin(p); err != nil {
		return err
	}
	if !domain.ValidKey(key) {
		return apperr.Unprocessable("invalid_key", "key must be 2-10 uppercase latin letters or digits, starting with a letter")
	}
	if strings.TrimSpace(name) == "" {
		return apperr.Unprocessable("invalid_name", "name is required")
	}
	_, err := s.repo.q.Exec(ctx, `INSERT INTO domains (key, name, approval_required) VALUES ($1,$2,$3)`, key, strings.TrimSpace(name), approvalRequired)
	if postgres.IsUniqueViolation(err) {
		return apperr.Conflict("key_exists", "a domain with this key already exists")
	}
	return err
}

// PatchDomain renames a domain or switches approval.
func (s *Service) PatchDomain(ctx context.Context, p *domain.Principal, key string, name *string, approvalRequired *bool) error {
	if err := requireAnyAdmin(p); err != nil {
		return err
	}
	if name != nil && strings.TrimSpace(*name) == "" {
		return apperr.Unprocessable("invalid_name", "name is required")
	}
	tag, err := s.repo.q.Exec(ctx, `UPDATE domains SET name = COALESCE($2, name), approval_required = COALESCE($3, approval_required) WHERE key = $1`,
		key, trimPtr(name), approvalRequired)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("domain_not_found", "domain not found")
	}
	if approvalRequired != nil {
		s.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{"domain": key}})
	}
	return nil
}

// SetApprovalAll switches approval for every domain; returns the number changed.
func (s *Service) SetApprovalAll(ctx context.Context, p *domain.Principal, approvalRequired bool) (int64, error) {
	if err := requireAnyAdmin(p); err != nil {
		return 0, err
	}
	tag, err := s.repo.q.Exec(ctx, `UPDATE domains SET approval_required = $1 WHERE approval_required <> $1`, approvalRequired)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		s.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{}})
	}
	return tag.RowsAffected(), nil
}

// CreateSystem adds a system to a domain.
func (s *Service) CreateSystem(ctx context.Context, p *domain.Principal, domainKey, key, name string) error {
	if err := requireAnyAdmin(p); err != nil {
		return err
	}
	if !domain.ValidKey(key) {
		return apperr.Unprocessable("invalid_key", "key must be 2-10 uppercase latin letters or digits, starting with a letter")
	}
	if strings.TrimSpace(name) == "" {
		return apperr.Unprocessable("invalid_name", "name is required")
	}
	tag, err := s.repo.q.Exec(ctx, `INSERT INTO systems (domain_id, key, name) SELECT id, $2, $3 FROM domains WHERE key = $1`,
		domainKey, key, strings.TrimSpace(name))
	if postgres.IsUniqueViolation(err) {
		return apperr.Conflict("key_exists", "a system with this key already exists in the domain")
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("domain_not_found", "domain not found")
	}
	return nil
}

// PatchSystem renames a system.
func (s *Service) PatchSystem(ctx context.Context, p *domain.Principal, domainKey, key, name string) error {
	if err := requireAnyAdmin(p); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return apperr.Unprocessable("invalid_name", "name is required")
	}
	tag, err := s.repo.q.Exec(ctx, `UPDATE systems SET name = $3 WHERE key = $2 AND domain_id = (SELECT id FROM domains WHERE key = $1)`,
		domainKey, key, strings.TrimSpace(name))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("system_not_found", "system not found")
	}
	return nil
}

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

// PublicRoutes mounts GET /api/v1/domains.
func (s *Service) PublicRoutes(r chi.Router) {
	r.Get("/domains", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		ds, err := s.repo.List(r.Context())
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, ds)
		return nil
	}))
}

// AdminRoutes mounts the admin dictionary routes.
func (s *Service) AdminRoutes(r chi.Router) {
	r.Get("/domains", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireAnyAdmin(p); err != nil {
			return err
		}
		ds, err := s.repo.List(r.Context())
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, ds)
		return nil
	}))
	r.Post("/domains", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			Key              string `json:"key"`
			Name             string `json:"name"`
			ApprovalRequired *bool  `json:"approvalRequired"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		ar := true
		if in.ApprovalRequired != nil {
			ar = *in.ApprovalRequired
		}
		if err := s.CreateDomain(r.Context(), p, in.Key, in.Name, ar); err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{"key": in.Key, "approvalRequired": ar})
		return nil
	}))
	r.Put("/domains/approval", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			ApprovalRequired bool `json:"approvalRequired"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		n, err := s.SetApprovalAll(r.Context(), p, in.ApprovalRequired)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, map[string]int64{"changed": n})
		return nil
	}))
	r.Patch("/domains/{key}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			Name             *string `json:"name"`
			ApprovalRequired *bool   `json:"approvalRequired"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.PatchDomain(r.Context(), p, chi.URLParam(r, "key"), in.Name, in.ApprovalRequired); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
	r.Post("/domains/{key}/systems", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.CreateSystem(r.Context(), p, chi.URLParam(r, "key"), in.Key, in.Name); err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]string{"key": in.Key})
		return nil
	}))
	r.Patch("/domains/{key}/systems/{systemKey}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			Name string `json:"name"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.PatchSystem(r.Context(), p, chi.URLParam(r, "key"), chi.URLParam(r, "systemKey"), in.Name); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
}
