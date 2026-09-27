// Package features implements feature creation, the feature list and card,
// feature deletion and edit locks.
package features

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// Service implements feature use cases.
type Service struct {
	store         specdata.Store
	git           git.Provider
	tokens        git.TokenSource
	events        events.Publisher
	defaultBranch string
}

// NewService creates the service.
func NewService(store specdata.Store, provider git.Provider, tokens git.TokenSource, ev events.Publisher, defaultBranch string) *Service {
	return &Service{store: store, git: provider, tokens: tokens, events: ev, defaultBranch: defaultBranch}
}

// CreateInput is the body of POST /features.
type CreateInput struct {
	Domain string  `json:"domain"`
	System string  `json:"system"`
	Title  string  `json:"title"`
	Parent *string `json:"parent"`
}

// ErrFeatureNotFound is the 404 for unknown features.
var ErrFeatureNotFound = apperr.NotFound("feature_not_found", "feature not found")

// Load returns a live feature: 404 if unknown, 410 if deleted.
func Load(ctx context.Context, store specdata.Store, uniqueID string) (*specdata.Feature, error) {
	f, err := store.FeatureByUniqueID(ctx, uniqueID)
	if errors.Is(err, specdata.ErrNotFound) {
		return nil, ErrFeatureNotFound
	}
	if err != nil {
		return nil, err
	}
	if f.Status == domain.FeatureDeleted {
		e := apperr.Gone("feature_deleted", "feature was deleted")
		e.With("uniqueId", f.UniqueID)
		if f.DeletedByName != nil {
			e.With("deletedBy", *f.DeletedByName)
		}
		if f.DeletedAt != nil {
			e.With("deletedAt", f.DeletedAt)
		}
		return nil, e
	}
	return f, nil
}

// Template reads the rules template of an area from the default branch.
// A missing template falls back to an empty document with a title.
func Template(ctx context.Context, provider git.Provider, token, defaultBranch string, area domain.Area, fix bool) string {
	f, err := provider.GetFile(ctx, token, defaultBranch, git.RulePath(string(area), fix))
	if err != nil {
		slog.WarnContext(ctx, "rules template unavailable, using a blank document", "area", area, "fix", fix, "err", err)
		if fix {
			return "---\nparent: <parent>\n---\n\n# <title>\n"
		}
		return "# <title>\n"
	}
	return string(f.Content)
}

// Create creates a feature: number, branch, first product gate from the rules
// template, and a PR/MR. The system row stays locked for the whole transaction,
// so concurrent creations get consecutive numbers; a failed git call rolls back
// and the number is not spent.
func (s *Service) Create(ctx context.Context, p *domain.Principal, in CreateInput) (*specdata.Feature, error) {
	if !p.Has(domain.RoleEditor, domain.AreaProduct) {
		return nil, apperr.NoRole("editor", "product")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" || len([]rune(title)) > 200 {
		return nil, apperr.Unprocessable("invalid_title", "title is required (up to 200 characters)")
	}
	var parent *specdata.Feature
	if in.Parent != nil && *in.Parent != "" {
		pf, err := s.store.FeatureByUniqueID(ctx, *in.Parent)
		if errors.Is(err, specdata.ErrNotFound) || (err == nil && pf.Status != domain.FeatureHandedOff) {
			return nil, apperr.Unprocessable("parent_not_handed_off", "a fix needs a parent feature that was handed off")
		}
		if err != nil {
			return nil, err
		}
		parent = pf
		// A fix gets its number in the parent's system.
		in.Domain, in.System = pf.DomainKey, pf.SystemKey
	}
	sys, err := s.store.SystemByKeys(ctx, in.Domain, in.System)
	if errors.Is(err, specdata.ErrNotFound) {
		return nil, apperr.Unprocessable("unknown_system", "domain or system is not in the dictionary")
	}
	if err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}

	var created *specdata.Feature
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		n, err := tx.NextNumber(ctx, sys.ID)
		if err != nil {
			return err
		}
		uid := domain.FormatUniqueID(sys.DomainKey, sys.Key, n)
		branch := git.FeatureBranch(uid)
		base, err := s.git.BranchHead(ctx, token, s.defaultBranch)
		if err != nil {
			return auth.MapGitError(err)
		}
		parentID := ""
		if parent != nil {
			parentID = parent.UniqueID
		}
		doc := markdown.RenderTemplate(Template(ctx, s.git, token, s.defaultBranch, domain.AreaProduct, parent != nil), title, parentID)
		if err := s.git.CreateBranch(ctx, token, branch, base); err != nil {
			return auth.MapGitError(err)
		}
		cleanup := func(cause error) error {
			if derr := s.git.DeleteBranch(context.WithoutCancel(ctx), token, branch); derr != nil {
				slog.ErrorContext(ctx, "delete branch after failed create", "branch", branch, "err", derr)
			}
			return auth.MapGitError(cause)
		}
		msg := git.Trailers{Feature: uid, Area: string(domain.AreaProduct)}.Message(fmt.Sprintf("%s: create feature", uid))
		sha, err := s.git.Commit(ctx, token, branch, msg,
			[]git.FileChange{{Path: git.SpecPath(sys.DomainKey, sys.Key, uid, string(domain.AreaProduct)), Content: []byte(doc)}})
		if err != nil {
			return cleanup(err)
		}
		body := fmt.Sprintf("Hammurapi feature **%s**: %s", uid, title)
		if parent != nil {
			body += fmt.Sprintf("\n\nFix of %s.", parent.UniqueID)
		}
		pr, err := s.git.CreatePR(ctx, token, branch, s.defaultBranch, fmt.Sprintf("%s %s", uid, title), body)
		if err != nil {
			return cleanup(err)
		}
		f := &specdata.Feature{UniqueID: uid, SystemID: sys.ID, DomainKey: sys.DomainKey, SystemKey: sys.Key,
			ApprovalRequired: sys.ApprovalRequired, Number: n, Title: title, Branch: branch, PRNumber: pr.Number, PRURL: pr.URL,
			Status: domain.FeatureInProgress, CreatedBy: p.UserID, CreatedByName: p.DisplayName}
		if parent != nil {
			f.ParentID, f.ParentUniqueID = &parent.ID, &parent.UniqueID
		}
		if err := tx.InsertFeature(ctx, f); err != nil {
			return err
		}
		g := &specdata.Gate{FeatureID: f.ID, Area: domain.AreaProduct, Status: domain.GateDraft, HeadCommit: sha, CreatedBy: p.UserID}
		if err := tx.InsertGate(ctx, g); err != nil {
			return err
		}
		if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventCreated, ActorID: &p.UserID, CommitSHA: &sha}); err != nil {
			return err
		}
		created = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Card is the feature card of GET /features/{id}.
type Card struct {
	UniqueID                 string                `json:"uniqueId"`
	Domain                   string                `json:"domain"`
	System                   string                `json:"system"`
	Title                    string                `json:"title"`
	Status                   domain.FeatureStatus  `json:"status"`
	ApprovalRequired         bool                  `json:"approvalRequired"`
	Branch                   string                `json:"branch"`
	PR                       PRRef                 `json:"pr"`
	Parent                   *string               `json:"parent"`
	Fixes                    []specdata.FeatureRef `json:"fixes"`
	Gates                    []specdata.GateDTO    `json:"gates"`
	Lock                     *specdata.LockDTO     `json:"lock"`
	CreatedBy                string                `json:"createdBy"`
	CreatedAt                any                   `json:"createdAt"`
	HandedOffBy              *string               `json:"handedOffBy"`
	HandedOffAt              any                   `json:"handedOffAt"`
	HandedOffWithoutApproval bool                  `json:"handedOffWithoutApproval"`
	Permissions              Permissions           `json:"permissions"`
}

// PRRef is a PR/MR reference.
type PRRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// Permissions tells the UI which actions the current user has.
type Permissions struct {
	Edit       []domain.Area `json:"edit"`
	Submit     []domain.Area `json:"submit"`
	Approve    []domain.Area `json:"approve"`
	DeleteGate []domain.Area `json:"deleteGate"`
	AddGate    []domain.Area `json:"addGate"`
	Handoff    bool          `json:"handoff"`
	Delete     bool          `json:"delete"`
}

// Get builds the feature card.
func (s *Service) Get(ctx context.Context, p *domain.Principal, uniqueID string) (*Card, error) {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	gates, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	fixes, err := s.store.Fixes(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	lock, err := s.store.GetLock(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	c := &Card{UniqueID: f.UniqueID, Domain: f.DomainKey, System: f.SystemKey, Title: f.Title, Status: f.Status,
		ApprovalRequired: f.ApprovalRequired, Branch: f.Branch, PR: PRRef{Number: f.PRNumber, URL: f.PRURL},
		Parent: f.ParentUniqueID, Fixes: fixes, Gates: specdata.GateDTOs(gates), Lock: specdata.ToLockDTO(lock),
		CreatedBy: f.CreatedByName, CreatedAt: f.CreatedAt, HandedOffBy: f.HandedOffByName, HandedOffAt: f.HandedOffAt,
		HandedOffWithoutApproval: f.HandedOffWithoutApproval}
	c.Permissions = permissions(p, f, gates)
	return c, nil
}

func permissions(p *domain.Principal, f *specdata.Feature, gates []specdata.Gate) Permissions {
	perm := Permissions{Edit: []domain.Area{}, Submit: []domain.Area{}, Approve: []domain.Area{}, DeleteGate: []domain.Area{}, AddGate: []domain.Area{}}
	if f.Status != domain.FeatureInProgress {
		return perm
	}
	active := map[domain.Area]bool{}
	for _, g := range gates {
		active[g.Area] = true
		if p.Has(domain.RoleEditor, g.Area) {
			perm.Edit = append(perm.Edit, g.Area)
			if len(gates) > 1 {
				perm.DeleteGate = append(perm.DeleteGate, g.Area)
			}
			if f.ApprovalRequired && g.Status == domain.GateDraft {
				perm.Submit = append(perm.Submit, g.Area)
			}
		}
		if f.ApprovalRequired && p.Has(domain.RoleApprover, g.Area) && g.Status == domain.GateInReview {
			perm.Approve = append(perm.Approve, g.Area)
		}
	}
	for _, a := range domain.Areas {
		if !active[a] && p.Has(domain.RoleEditor, a) {
			perm.AddGate = append(perm.AddGate, a)
		}
	}
	perm.Handoff = specdata.HasAnyRoleIn(p, gates) && (!f.ApprovalRequired || specdata.AllApproved(gates))
	perm.Delete = p.GlobalAdmin || specdata.EditorOfAll(p, gates)
	return perm
}

// Delete deletes a feature before hand-off: closes the PR/MR without merging
// and deletes the branch. The row is kept (soft delete), the number is never reused.
func (s *Service) Delete(ctx context.Context, p *domain.Principal, uniqueID, confirm string) error {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return err
	}
	if f.Status == domain.FeatureHandedOff {
		return apperr.Conflict("feature_handed_off", "a handed-off feature cannot be deleted; create a fix instead")
	}
	gates, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return err
	}
	if !p.GlobalAdmin && !specdata.EditorOfAll(p, gates) {
		return apperr.Forbidden("forbidden", "editor role in every area of the feature or global admin required")
	}
	if confirm != f.UniqueID {
		return apperr.Unprocessable("confirm_mismatch", "confirmation does not match the feature id")
	}
	if err := LockedByOther(ctx, s.store, f.ID, p.UserID); err != nil {
		return err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return err
	}
	if err := s.git.ClosePR(ctx, token, f.PRNumber); err != nil && !errors.Is(err, git.ErrNotFound) {
		return auth.MapGitError(err)
	}
	pending := false
	if err := s.git.DeleteBranch(ctx, token, f.Branch); err != nil {
		slog.WarnContext(ctx, "branch deletion failed, cleaner will retry", "branch", f.Branch, "err", err)
		pending = true
	}
	if err := s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := tx.MarkDeleted(ctx, f.ID, p.UserID, pending); err != nil {
			return err
		}
		return tx.DropLock(ctx, f.ID)
	}); err != nil {
		return err
	}
	s.events.Publish(ctx, events.Event{Type: events.FeatureDeleted, Data: map[string]string{"uniqueId": f.UniqueID}})
	s.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{"uniqueId": f.UniqueID}})
	return nil
}

// LockedByOther returns 423 if another user holds the feature lock.
func LockedByOther(ctx context.Context, store specdata.Store, featureID, userID uuid.UUID) error {
	l, err := store.GetLock(ctx, featureID)
	if err != nil {
		return err
	}
	if l != nil && l.LockedBy != userID {
		return apperr.Locked("feature_locked", "feature is being edited by "+l.LockedByName).
			With("userName", l.LockedByName).With("lockedAt", l.LockedAt)
	}
	return nil
}

// Lock takes or extends the edit lock.
func (s *Service) Lock(ctx context.Context, p *domain.Principal, uniqueID string) (*specdata.Lock, error) {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	if f.Status != domain.FeatureInProgress {
		return nil, apperr.Conflict("feature_handed_off", "feature is read-only")
	}
	l, ok, err := s.store.AcquireLock(ctx, f.ID, p.UserID)
	if err != nil {
		return nil, err
	}
	if !ok {
		e := apperr.Conflict("feature_locked", "feature is being edited by another user")
		if l != nil {
			e.With("userName", l.LockedByName).With("lockedAt", l.LockedAt)
		}
		return nil, e
	}
	return l, nil
}

// Unlock releases the caller's lock.
func (s *Service) Unlock(ctx context.Context, p *domain.Principal, uniqueID string) error {
	f, err := s.store.FeatureByUniqueID(ctx, uniqueID)
	if errors.Is(err, specdata.ErrNotFound) {
		return ErrFeatureNotFound
	}
	if err != nil {
		return err
	}
	return s.store.ReleaseLock(ctx, f.ID, p.UserID)
}

// List returns the home page feature list.
func (s *Service) List(ctx context.Context, lf specdata.ListFilter) ([]specdata.ListedFeature, error) {
	return s.store.ListFeatures(ctx, lf)
}
