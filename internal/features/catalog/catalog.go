// Package catalog projects the Backstage catalog (catalog-info.yaml in git) onto
// domains, systems and services (PLT.HMR-0002 R10–R11, arch §10): Domain and
// System entities from the catalog repository (glob), Component entities from
// the service repositories; owners resolved from spec.owner (user:<login> or
// group:<name> of the provider). Entities that cannot be mapped go to
// catalog_errors; entities removed from the catalog but still referenced are
// marked "deleted in catalog".
package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Annotations.
const (
	AnnotationKey            = "hammurapi/key"
	AnnotationDeployWorkflow = "hammurapi/deploy-workflow"
)

// Entity is a Backstage entity.
type Entity struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Title       string            `yaml:"title"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Owner  string `yaml:"owner"`
		Domain string `yaml:"domain"`
		System string `yaml:"system"`
		Type   string `yaml:"type"`
	} `yaml:"spec"`
	Ref  string `yaml:"-"` // repo:path
	Repo string `yaml:"-"`
}

// Parse reads a (multi-document) catalog-info.yaml.
func Parse(data []byte, repo, file string) ([]Entity, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []Entity
	for {
		var e Entity
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("%s:%s: %w", repo, file, err)
		}
		if e.Kind == "" {
			continue
		}
		e.Ref, e.Repo = repo+":"+file, repo
		out = append(out, e)
	}
}

// Key derives the Hammurapi key: the hammurapi/key annotation, otherwise
// metadata.name in upper case when it fits the key format (R10, CAT-02).
func Key(e Entity) (string, bool) {
	if k := strings.TrimSpace(e.Metadata.Annotations[AnnotationKey]); k != "" {
		return k, domain.ValidKey(k)
	}
	k := strings.ToUpper(e.Metadata.Name)
	return k, domain.ValidKey(k)
}

func title(e Entity) string {
	if e.Metadata.Title != "" {
		return e.Metadata.Title
	}
	return e.Metadata.Name
}

// strip "kind:namespace/name" references to the name.
func refName(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

var serviceKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// MatchGlob matches a path against a glob with ** (any number of directories).
func MatchGlob(glob, p string) bool {
	if glob == "" {
		glob = "**/catalog-info.yaml"
	}
	re := regexp.QuoteMeta(glob)
	re = strings.ReplaceAll(re, `\*\*/`, `(?:.*/)?`)
	re = strings.ReplaceAll(re, `\*\*`, `.*`)
	re = strings.ReplaceAll(re, `\*`, `[^/]*`)
	re = strings.ReplaceAll(re, `\?`, `[^/]`)
	ok, _ := regexp.MatchString("^"+re+"$", p)
	return ok
}

// Syncer runs synchronizations.
type Syncer struct {
	Pool *pgxpool.Pool
	Git  git.Provider
	mu   sync.Mutex
}

// Result of a synchronization.
type Result struct {
	Domains  int      `json:"domains"`
	Systems  int      `json:"systems"`
	Services int      `json:"services"`
	Errors   int      `json:"errors"`
	Deleted  []string `json:"deleted"`
}

// Changed is called for pushes touching yaml files (CAT-05): the catalog is
// re-synchronized when a catalog file changed.
func (s *Syncer) Changed(ctx context.Context, repo string, paths []string) error {
	var set cycledata.CatalogSetting
	if _, err := cycledata.New(s.Pool).Setting(ctx, "catalog", &set); err != nil || !set.Enabled {
		return err
	}
	relevant := false
	for _, p := range paths {
		if strings.EqualFold(repo, set.CatalogRepo) && MatchGlob(set.CatalogGlob, p) {
			relevant = true
		}
		if p == serviceFile(set) {
			for _, r := range set.ServiceRepos {
				if strings.EqualFold(r, repo) {
					relevant = true
				}
			}
		}
	}
	if !relevant {
		return nil
	}
	_, err := s.Sync(ctx)
	return err
}

func serviceFile(set cycledata.CatalogSetting) string {
	if set.ServiceFilePath == "" {
		return "catalog-info.yaml"
	}
	return strings.TrimPrefix(set.ServiceFilePath, "/")
}

func (s *Syncer) read(ctx context.Context, repo, file string) ([]byte, error) {
	p := s.Git.ForRepo(repo)
	token, err := p.BotToken(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := p.DefaultBranch(ctx, token)
	if err != nil {
		return nil, err
	}
	f, err := p.GetFile(ctx, token, ref, file)
	if err != nil {
		return nil, err
	}
	return f.Content, nil
}

// Collect reads all entities from git.
func (s *Syncer) Collect(ctx context.Context, set cycledata.CatalogSetting) ([]Entity, []error) {
	var out []Entity
	var errs []error
	if set.CatalogRepo != "" {
		p := s.Git.ForRepo(set.CatalogRepo)
		token, err := p.BotToken(ctx)
		if err == nil {
			ref, err := p.DefaultBranch(ctx, token)
			if err != nil {
				errs = append(errs, err)
			} else if files, err := p.ListFiles(ctx, token, ref, ""); err != nil {
				errs = append(errs, err)
			} else {
				for _, f := range files {
					if !MatchGlob(set.CatalogGlob, f) {
						continue
					}
					file, err := p.GetFile(ctx, token, ref, f)
					if err != nil {
						errs = append(errs, err)
						continue
					}
					es, err := Parse(file.Content, set.CatalogRepo, f)
					if err != nil {
						errs = append(errs, err)
					}
					out = append(out, es...)
				}
			}
		} else {
			errs = append(errs, err)
		}
	}
	for _, repo := range set.ServiceRepos {
		data, err := s.read(ctx, repo, serviceFile(set))
		if errors.Is(err, git.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
			continue
		}
		es, err := Parse(data, repo, serviceFile(set))
		if err != nil {
			errs = append(errs, err)
		}
		out = append(out, es...)
	}
	return out, errs
}

// Sync runs a full synchronization.
func (s *Syncer) Sync(ctx context.Context) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cd := cycledata.New(s.Pool)
	var set cycledata.CatalogSetting
	if _, err := cd.Setting(ctx, "catalog", &set); err != nil {
		return nil, err
	}
	if !set.Enabled {
		return nil, apperr.Conflict("catalog_disabled", "the Backstage integration is disabled")
	}
	entities, readErrs := s.Collect(ctx, set)
	res, err := s.Apply(ctx, entities)
	now := time.Now()
	set.LastSyncAt = &now
	set.LastSyncError = nil
	if err != nil {
		msg := err.Error()
		set.LastSyncError = &msg
	} else if len(readErrs) > 0 {
		msg := readErrs[0].Error()
		set.LastSyncError = &msg
	}
	if perr := cd.PutSetting(context.WithoutCancel(ctx), "catalog", set, nil); perr != nil && err == nil {
		err = perr
	}
	return res, err
}

type catErr struct{ kind, name, ref, reason string }

// Apply writes the projection of the entities in one transaction.
func (s *Syncer) Apply(ctx context.Context, entities []Entity) (*Result, error) {
	res := &Result{Deleted: []string{}}
	var errs []catErr
	owners := map[uuid.UUID]string{}
	err := postgres.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		cd := cycledata.New(tx)
		domainByName := map[string]uuid.UUID{}
		seenDomains, seenSystems, seenServices := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
		for _, e := range entities {
			if e.Kind != "Domain" {
				continue
			}
			key, ok := Key(e)
			if !ok {
				errs = append(errs, catErr{"Domain", e.Metadata.Name, e.Ref, "the name is not a valid key and there is no hammurapi/key annotation"})
				continue
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO domains (key, name, source, catalog_name, catalog_ref) VALUES ($1,$2,'backstage',$3,$4)
				ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, source = 'backstage', catalog_name = EXCLUDED.catalog_name,
				catalog_ref = EXCLUDED.catalog_ref, deleted_in_catalog = false RETURNING id`, key, title(e), e.Metadata.Name, e.Ref).Scan(&id); err != nil {
				return err
			}
			domainByName[e.Metadata.Name] = id
			seenDomains[id] = true
			res.Domains++
		}
		systemByName := map[string]uuid.UUID{}
		for _, e := range entities {
			if e.Kind != "System" {
				continue
			}
			key, ok := Key(e)
			if !ok {
				errs = append(errs, catErr{"System", e.Metadata.Name, e.Ref, "the name is not a valid key and there is no hammurapi/key annotation"})
				continue
			}
			did, ok := domainByName[refName(e.Spec.Domain)]
			if !ok {
				errs = append(errs, catErr{"System", e.Metadata.Name, e.Ref, "spec.domain " + e.Spec.Domain + " is not a known Domain"})
				continue
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO systems (domain_id, key, name, source, catalog_name, catalog_ref) VALUES ($1,$2,$3,'backstage',$4,$5)
				ON CONFLICT (domain_id, key) DO UPDATE SET name = EXCLUDED.name, source = 'backstage', catalog_name = EXCLUDED.catalog_name,
				catalog_ref = EXCLUDED.catalog_ref, deleted_in_catalog = false RETURNING id`, did, key, title(e), e.Metadata.Name, e.Ref).Scan(&id); err != nil {
				return err
			}
			systemByName[e.Metadata.Name] = id
			seenSystems[id] = true
			res.Systems++
		}
		for _, e := range entities {
			if e.Kind != "Component" {
				continue
			}
			key := e.Metadata.Name
			if !serviceKeyRe.MatchString(key) {
				errs = append(errs, catErr{"Component", e.Metadata.Name, e.Ref, "metadata.name is not a valid service key"})
				continue
			}
			var sysID *uuid.UUID
			if e.Spec.System != "" {
				id, ok := systemByName[refName(e.Spec.System)]
				if !ok {
					errs = append(errs, catErr{"Component", e.Metadata.Name, e.Ref, "spec.system " + e.Spec.System + " is not a known System"})
					continue
				}
				sysID = &id
			}
			repo := e.Repo
			for _, a := range []string{"github.com/project-slug", "gitlab.com/project-slug"} {
				if v := e.Metadata.Annotations[a]; v != "" {
					repo = v
				}
			}
			svc := &cycledata.Service{Key: key, Name: title(e), SystemID: sysID, Repo: repo, OwnerRef: e.Spec.Owner, Source: "backstage", CatalogRef: &e.Ref}
			if wf := e.Metadata.Annotations[AnnotationDeployWorkflow]; wf != "" {
				svc.DeployOverride = []byte(fmt.Sprintf(`{"production":{"workflow":%q},"stage":{"workflow":%q}}`, wf, wf))
				svc.OverrideFromCatalog = true // DEP-04
			}
			if err := cd.UpsertService(ctx, svc); err != nil {
				return err
			}
			seenServices[svc.ID] = true
			owners[svc.ID] = e.Spec.Owner
			res.Services++
		}
		// Entities gone from the catalog (CAT-06): deleted unless referenced, otherwise marked.
		if err := s.removeMissing(ctx, tx, res, seenDomains, seenSystems, seenServices); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM catalog_errors`); err != nil {
			return err
		}
		for _, ce := range errs {
			if _, err := tx.Exec(ctx, `INSERT INTO catalog_errors (kind, name, catalog_ref, reason) VALUES ($1,$2,$3,$4)
				ON CONFLICT (kind, name) DO UPDATE SET catalog_ref = EXCLUDED.catalog_ref, reason = EXCLUDED.reason, seen_at = now()`,
				ce.kind, ce.name, ce.ref, ce.reason); err != nil {
				return err
			}
		}
		res.Errors = len(errs)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Owners are resolved after the commit: group lookups call the provider.
	for id, ref := range owners {
		users, err := s.ResolveOwners(ctx, ref)
		if err != nil {
			slog.WarnContext(ctx, "catalog owner not resolved", "owner", ref, "err", err)
			continue
		}
		if err := cycledata.New(s.Pool).SetServiceOwners(ctx, id, users); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (s *Syncer) removeMissing(ctx context.Context, tx pgx.Tx, res *Result, domains, systems, services map[uuid.UUID]bool) error {
	cd := cycledata.New(tx)
	rows, err := tx.Query(ctx, `SELECT id, key FROM services WHERE source = 'backstage'`)
	if err != nil {
		return err
	}
	type row struct {
		id  uuid.UUID
		key string
	}
	var gone []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key); err != nil {
			rows.Close()
			return err
		}
		if !services[r.id] {
			gone = append(gone, r)
		}
	}
	rows.Close()
	for _, r := range gone {
		if _, err := cd.DeleteService(ctx, r.id); err != nil {
			return err
		}
		res.Deleted = append(res.Deleted, "Component:"+r.key)
	}
	for _, t := range []struct {
		table, kind string
		seen        map[uuid.UUID]bool
		refs        string
	}{
		{"systems", "System", systems, `EXISTS (SELECT 1 FROM features f WHERE f.system_id = x.id) OR EXISTS (SELECT 1 FROM services s WHERE s.system_id = x.id)`},
		{"domains", "Domain", domains, `EXISTS (SELECT 1 FROM systems s WHERE s.domain_id = x.id) OR EXISTS (SELECT 1 FROM issues i WHERE i.domain_id = x.id)`},
	} {
		rows, err := tx.Query(ctx, `SELECT x.id, x.key, `+t.refs+` FROM `+t.table+` x WHERE x.source = 'backstage'`)
		if err != nil {
			return err
		}
		type ent struct {
			id   uuid.UUID
			key  string
			used bool
		}
		var list []ent
		for rows.Next() {
			var e ent
			if err := rows.Scan(&e.id, &e.key, &e.used); err != nil {
				rows.Close()
				return err
			}
			if !t.seen[e.id] {
				list = append(list, e)
			}
		}
		rows.Close()
		for _, e := range list {
			if e.used {
				if _, err := tx.Exec(ctx, `UPDATE `+t.table+` SET deleted_in_catalog = true WHERE id = $1`, e.id); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `DELETE FROM `+t.table+` WHERE id = $1`, e.id); err != nil {
				return err
			}
			res.Deleted = append(res.Deleted, t.kind+":"+e.key)
		}
	}
	return nil
}

// ResolveOwners resolves spec.owner: user:<login> or group:<name> (CAT-04).
func (s *Syncer) ResolveOwners(ctx context.Context, ref string) ([]uuid.UUID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	kind, name, ok := strings.Cut(ref, ":")
	if !ok {
		kind, name = "user", ref
	}
	name = refName(name)
	var logins []string
	switch kind {
	case "user":
		logins = []string{name}
	case "group":
		token, err := s.Git.BotToken(ctx)
		if err != nil {
			return nil, err
		}
		if logins, err = s.Git.GroupMembers(ctx, token, name); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported owner %q", ref)
	}
	cd := cycledata.New(s.Pool)
	var out []uuid.UUID
	for _, l := range logins {
		if id, err := cd.UserByUsername(ctx, l); err == nil && id != nil {
			out = append(out, *id)
		}
	}
	return out, nil
}

// ─── Admin API ──────────────────────────────────────────────────────

// Routes mounts /admin/api/v1/catalog.
func (s *Syncer) Routes(r chi.Router) {
	global := func(r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if !p.GlobalAdmin {
			return apperr.Forbidden("forbidden", "global administrator role required")
		}
		return nil
	}
	r.Get("/catalog", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if err := global(r); err != nil {
			return err
		}
		var set cycledata.CatalogSetting
		if _, err := cycledata.New(s.Pool).Setting(r.Context(), "catalog", &set); err != nil {
			return err
		}
		if set.ServiceRepos == nil {
			set.ServiceRepos = []string{}
		}
		httpx.JSON(w, 200, set)
		return nil
	}))
	r.Put("/catalog", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if err := global(r); err != nil {
			return err
		}
		p, _ := httpx.MustPrincipal(r)
		var in cycledata.CatalogSetting
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		cd := cycledata.New(s.Pool)
		var cur cycledata.CatalogSetting
		if _, err := cd.Setting(r.Context(), "catalog", &cur); err != nil {
			return err
		}
		in.LastSyncAt, in.LastSyncError = cur.LastSyncAt, cur.LastSyncError
		if in.CatalogGlob == "" {
			in.CatalogGlob = "**/catalog-info.yaml"
		}
		if in.ServiceFilePath == "" {
			in.ServiceFilePath = "catalog-info.yaml"
		}
		for _, x := range append([]string{in.CatalogGlob}, in.ServiceFilePath) {
			if _, err := path.Match(strings.ReplaceAll(x, "**", "*"), "x"); err != nil {
				return apperr.Unprocessable("invalid_glob", err.Error())
			}
		}
		if in.ServiceRepos == nil {
			in.ServiceRepos = []string{}
		}
		if err := cd.PutSetting(r.Context(), "catalog", in, &p.UserID); err != nil {
			return err
		}
		httpx.JSON(w, 200, in)
		return nil
	}))
	r.Post("/catalog/sync", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if err := global(r); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		res, err := s.Sync(ctx)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, res)
		return nil
	}))
	r.Get("/catalog/errors", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if err := global(r); err != nil {
			return err
		}
		rows, err := s.Pool.Query(r.Context(), `SELECT kind, name, catalog_ref, reason, seen_at FROM catalog_errors ORDER BY kind, name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		type item struct {
			Kind   string    `json:"kind"`
			Name   string    `json:"name"`
			Ref    string    `json:"catalogRef"`
			Reason string    `json:"reason"`
			SeenAt time.Time `json:"seenAt"`
		}
		out := []item{}
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.Kind, &it.Name, &it.Ref, &it.Reason, &it.SeenAt); err != nil {
				return err
			}
			out = append(out, it)
		}
		httpx.JSON(w, 200, out)
		return rows.Err()
	}))
}
