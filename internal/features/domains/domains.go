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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// System is a system in the dictionary.
type System struct {
	Key              string `json:"key"`
	Name             string `json:"name"`
	LastNumber       int    `json:"lastNumber"`
	Source           string `json:"source"`
	DeletedInCatalog bool   `json:"deletedInCatalog"`
}

// ExpertUser is an expert of a domain.
type ExpertUser struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
}

// Experts are the product and technical experts of a domain.
type Experts struct {
	Product   []ExpertUser `json:"product"`
	Technical []ExpertUser `json:"technical"`
}

// Domain is a domain with its systems.
type Domain struct {
	ID               uuid.UUID `json:"-"`
	Key              string    `json:"key"`
	Name             string    `json:"name"`
	ApprovalRequired bool      `json:"approvalRequired"`
	Source           string    `json:"source"`
	DeletedInCatalog bool      `json:"deletedInCatalog"`
	CreatedAt        time.Time `json:"createdAt"`
	Systems          []System  `json:"systems"`
	Experts          Experts   `json:"experts"`
}

// Repository persists the dictionary.
type Repository struct{ q postgres.Querier }

// NewRepository creates a repository.
func NewRepository(q postgres.Querier) *Repository { return &Repository{q: q} }

// List returns all domains with systems.
func (r *Repository) List(ctx context.Context) ([]Domain, error) {
	rows, err := r.q.Query(ctx, `SELECT d.id, d.key, d.name, d.approval_required, d.source::text, d.deleted_in_catalog, d.created_at,
		s.key, s.name, s.last_number, s.source::text, s.deleted_in_catalog
		FROM domains d LEFT JOIN systems s ON s.domain_id = d.id ORDER BY d.key, s.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Domain{}
	idx := map[uuid.UUID]int{}
	for rows.Next() {
		var d Domain
		var sk, sn, ss *string
		var ln *int
		var sdel *bool
		if err := rows.Scan(&d.ID, &d.Key, &d.Name, &d.ApprovalRequired, &d.Source, &d.DeletedInCatalog, &d.CreatedAt, &sk, &sn, &ln, &ss, &sdel); err != nil {
			return nil, err
		}
		i, ok := idx[d.ID]
		if !ok {
			d.Systems = []System{}
			d.Experts = Experts{Product: []ExpertUser{}, Technical: []ExpertUser{}}
			out = append(out, d)
			i = len(out) - 1
			idx[d.ID] = i
		}
		if sk != nil {
			out[i].Systems = append(out[i].Systems, System{Key: *sk, Name: *sn, LastNumber: *ln, Source: *ss, DeletedInCatalog: *sdel})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	er, err := r.q.Query(ctx, `SELECT e.domain_id, e.kind::text, u.id, u.username, u.display_name
		FROM domain_experts e JOIN users u ON u.id = e.user_id ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer er.Close()
	for er.Next() {
		var did uuid.UUID
		var kind string
		var u ExpertUser
		if err := er.Scan(&did, &kind, &u.ID, &u.Username, &u.DisplayName); err != nil {
			return nil, err
		}
		i, ok := idx[did]
		if !ok {
			continue
		}
		if domain.ExpertKind(kind) == domain.ExpertProduct {
			out[i].Experts.Product = append(out[i].Experts.Product, u)
		} else {
			out[i].Experts.Technical = append(out[i].Experts.Technical, u)
		}
	}
	return out, er.Err()
}

// CatalogManaged reports whether the dictionary comes from Backstage (R24).
func CatalogManaged(ctx context.Context, q postgres.Querier) (bool, error) {
	var c cycledata.CatalogSetting
	if _, err := cycledata.New(q).Setting(ctx, "catalog", &c); err != nil {
		return false, err
	}
	return c.Enabled, nil
}

// RequireManual returns 409 catalog_managed when Backstage integration is on.
func RequireManual(ctx context.Context, q postgres.Querier) error {
	managed, err := CatalogManaged(ctx, q)
	if err != nil {
		return err
	}
	if managed {
		return apperr.Conflict("catalog_managed", "domains, systems and services are managed in the Backstage catalog")
	}
	return nil
}

// ExpertsInput is the body of PUT /admin/api/v1/domains/{key}/experts.
type ExpertsInput struct {
	Product   []uuid.UUID `json:"product"`
	Technical []uuid.UUID `json:"technical"`
}

// SetExperts replaces the experts of a domain. Any administrator; experts are
// assigned in Hammurapi even when the dictionary comes from Backstage.
func (s *Service) SetExperts(ctx context.Context, p *domain.Principal, key string, in ExpertsInput) error {
	if err := requireAnyAdmin(p); err != nil {
		return err
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM domains WHERE key = $1`, key).Scan(&id); err != nil {
			if postgres.IsNoRows(err) {
				return apperr.NotFound("domain_not_found", "domain not found")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM domain_experts WHERE domain_id = $1`, id); err != nil {
			return err
		}
		for kind, users := range map[domain.ExpertKind][]uuid.UUID{domain.ExpertProduct: in.Product, domain.ExpertTechnical: in.Technical} {
			for _, u := range users {
				if _, err := tx.Exec(ctx, `INSERT INTO domain_experts (domain_id, user_id, kind) SELECT $1, id, $3 FROM users WHERE id = $2
					ON CONFLICT DO NOTHING`, id, u, kind); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Service implements dictionary use cases.
type Service struct {
	pool   *pgxpool.Pool
	repo   *Repository
	events events.Publisher
	// CatalogChanged runs in the transaction that adds a domain or a system:
	// specifications waiting for it get an extra check (FTR.HMR.CMN-0005 R6).
	CatalogChanged func(ctx context.Context, q postgres.Querier) error
}

// added runs the insert and the catalog hook in one transaction.
func (s *Service) added(ctx context.Context, insert func(q postgres.Querier) error) error {
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := insert(tx); err != nil {
			return err
		}
		if s.CatalogChanged != nil {
			return s.CatalogChanged(ctx, tx)
		}
		return nil
	})
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, ev events.Publisher) *Service {
	return &Service{pool: pool, repo: NewRepository(pool), events: ev}
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
	if err := RequireManual(ctx, s.repo.q); err != nil {
		return err
	}
	err := s.added(ctx, func(q postgres.Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO domains (key, name, approval_required) VALUES ($1,$2,$3)`, key, strings.TrimSpace(name), approvalRequired)
		return err
	})
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
	if name != nil {
		// approvalRequired is always editable; the name comes from the catalog.
		if err := RequireManual(ctx, s.repo.q); err != nil {
			return err
		}
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
	if err := RequireManual(ctx, s.repo.q); err != nil {
		return err
	}
	if !domain.ValidKey(key) {
		return apperr.Unprocessable("invalid_key", "key must be 2-10 uppercase latin letters or digits, starting with a letter")
	}
	if strings.TrimSpace(name) == "" {
		return apperr.Unprocessable("invalid_name", "name is required")
	}
	err := s.added(ctx, func(q postgres.Querier) error {
		tag, err := q.Exec(ctx, `INSERT INTO systems (domain_id, key, name) SELECT id, $2, $3 FROM domains WHERE key = $1`,
			domainKey, key, strings.TrimSpace(name))
		if err == nil && tag.RowsAffected() == 0 {
			return apperr.NotFound("domain_not_found", "domain not found")
		}
		return err
	})
	if postgres.IsUniqueViolation(err) {
		return apperr.Conflict("key_exists", "a system with this key already exists in the domain")
	}
	return err
}

// PatchSystem renames a system.
func (s *Service) PatchSystem(ctx context.Context, p *domain.Principal, domainKey, key, name string) error {
	if err := requireAnyAdmin(p); err != nil {
		return err
	}
	if err := RequireManual(ctx, s.repo.q); err != nil {
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
	r.Put("/domains/{key}/experts", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in ExpertsInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.SetExperts(r.Context(), p, chi.URLParam(r, "key"), in); err != nil {
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
