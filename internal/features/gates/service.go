// Package gates implements gate documents: adding and deleting gates, reading
// and saving documents, diff since approval and gate history.
package gates

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// Service implements gate use cases.
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

// Errors.
var (
	ErrGateNotFound = apperr.NotFound("gate_not_found", "gate not found")
	ErrReadOnly     = apperr.Conflict("feature_handed_off", "the feature was handed off; changes go through a fix feature")
)

func (s *Service) loadGate(ctx context.Context, uniqueID string, area domain.Area) (*specdata.Feature, *specdata.Gate, error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, nil, err
	}
	g, err := s.store.ActiveGate(ctx, f.ID, area)
	if errors.Is(err, specdata.ErrNotFound) {
		return f, nil, ErrGateNotFound
	}
	return f, g, err
}

func specPath(f *specdata.Feature, area domain.Area) string {
	return git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, string(area))
}

// readRef is the branch while the feature is in progress; after hand-off the
// branch may be gone, and the merged content lives in the default branch.
func (s *Service) readRef(f *specdata.Feature) string {
	if f.Status == domain.FeatureInProgress {
		return f.Branch
	}
	return s.defaultBranch
}

// AddGate adds a gate whose document is created from the area template.
func (s *Service) AddGate(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*specdata.Gate, error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	if !p.Has(domain.RoleEditor, area) {
		return nil, apperr.NoRole("editor", string(area))
	}
	if f.Status != domain.FeatureInProgress {
		return nil, ErrReadOnly
	}
	if _, err := s.store.ActiveGate(ctx, f.ID, area); err == nil {
		return nil, apperr.Conflict("gate_exists", "the gate already exists")
	} else if !errors.Is(err, specdata.ErrNotFound) {
		return nil, err
	}
	if err := features.LockedByOther(ctx, s.store, f.ID, p.UserID); err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	parentID := ""
	if f.ParentUniqueID != nil {
		parentID = *f.ParentUniqueID
	}
	doc := markdown.RenderTemplate(features.Template(ctx, s.git, token, s.defaultBranch, area, f.IsFix()), f.Title, parentID)
	msg := git.Trailers{Feature: f.UniqueID, Area: string(area)}.Message(fmt.Sprintf("%s: add %s specification", f.UniqueID, area))
	sha, err := s.git.Commit(ctx, token, f.Branch, msg, []git.FileChange{{Path: specPath(f, area), Content: []byte(doc)}})
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	g := &specdata.Gate{FeatureID: f.ID, Area: area, Status: domain.GateDraft, HeadCommit: sha, CreatedBy: p.UserID}
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := tx.InsertGate(ctx, g); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventCreated, ActorID: &p.UserID, CommitSHA: &sha})
	})
	if err != nil {
		return nil, err
	}
	s.publishGate(ctx, f, g)
	return g, nil
}

// DeleteGate deletes the area folder from the feature branch.
func (s *Service) DeleteGate(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) error {
	f, g, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return err
	}
	if !p.Has(domain.RoleEditor, area) {
		return apperr.NoRole("editor", string(area))
	}
	if f.Status != domain.FeatureInProgress {
		return ErrReadOnly
	}
	gates, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return err
	}
	if len(gates) <= 1 {
		return apperr.Conflict("last_gate", "the last specification cannot be deleted; delete the feature instead")
	}
	if err := features.LockedByOther(ctx, s.store, f.ID, p.UserID); err != nil {
		return err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return err
	}
	dir := git.SpecDir(f.DomainKey, f.SystemKey, f.UniqueID, string(area))
	files, err := s.git.ListFiles(ctx, token, f.Branch, dir)
	if err != nil {
		return auth.MapGitError(err)
	}
	if len(files) == 0 {
		files = []string{dir + "/spec.md"}
	}
	changes := make([]git.FileChange, 0, len(files))
	for _, path := range files {
		changes = append(changes, git.FileChange{Path: path, Delete: true})
	}
	msg := git.Trailers{Feature: f.UniqueID, Area: string(area), Delete: string(area)}.Message(fmt.Sprintf("%s: delete %s specification", f.UniqueID, area))
	sha, err := s.git.Commit(ctx, token, f.Branch, msg, changes)
	if err != nil {
		return auth.MapGitError(err)
	}
	wasInReview := g.Status == domain.GateInReview
	now := time.Now()
	g.DeletedAt, g.DeletedBy, g.HeadCommit = &now, &p.UserID, sha
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := tx.SaveGate(ctx, g); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventDeleted, ActorID: &p.UserID, CommitSHA: &sha})
	})
	if err != nil {
		return err
	}
	s.publishGate(ctx, f, g)
	if wasInReview {
		s.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{"uniqueId": f.UniqueID}})
	}
	return nil
}

func (s *Service) publishGate(ctx context.Context, f *specdata.Feature, g *specdata.Gate) {
	PublishGateUpdated(ctx, s.events, f.UniqueID, g)
}

// PublishGateUpdated emits gate.updated.
func PublishGateUpdated(ctx context.Context, ev events.Publisher, uniqueID string, g *specdata.Gate) {
	ev.Publish(ctx, events.Event{Type: events.GateUpdated, Data: map[string]any{
		"uniqueId": uniqueID, "area": g.Area, "status": g.Status, "headCommit": g.HeadCommit, "deleted": g.DeletedAt != nil,
	}})
}

// Document is a gate document.
type Document struct {
	Content  string            `json:"content"`
	SHA      string            `json:"sha"`
	Fix      bool              `json:"fix"`
	Warnings []string          `json:"warnings"`
	Lock     *specdata.LockDTO `json:"lock"`
	ReadOnly bool              `json:"readOnly"`
}

// GetDocument reads the gate document from git.
func (s *Service) GetDocument(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*Document, error) {
	f, _, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	file, err := s.git.GetFile(ctx, token, s.readRef(f), specPath(f, area))
	if errors.Is(err, git.ErrNotFound) {
		return nil, apperr.NotFound("document_not_found", "the document is missing in git")
	}
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	lock, err := s.store.GetLock(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	content := string(file.Content)
	ro := f.Status != domain.FeatureInProgress || !p.Has(domain.RoleEditor, area) || (lock != nil && lock.LockedBy != p.UserID)
	warn := markdown.CheckSubset(content, f.IsFix())
	if warn == nil {
		warn = []string{}
	}
	return &Document{Content: content, SHA: git.BlobSHA(file.Content), Fix: f.IsFix(), Warnings: warn, Lock: specdata.ToLockDTO(lock), ReadOnly: ro}, nil
}

// SaveInput is the body of PUT document.
type SaveInput struct {
	Content string `json:"content"`
	BaseSHA string `json:"baseSha"`
}

// SaveResult is returned after a save.
type SaveResult struct {
	SHA    string  `json:"sha"`
	Commit *string `json:"commit"`
}

// SaveDocument commits a new version of the gate document on behalf of the
// user. The agent uses the same path with isAgent=true. The projection (event
// "edited", reset to draft) is written by the worker from the push webhook.
func (s *Service) SaveDocument(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area, in SaveInput, isAgent bool) (*SaveResult, error) {
	f, g, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	if !p.Has(domain.RoleEditor, area) {
		return nil, apperr.NoRole("editor", string(area))
	}
	if f.Status != domain.FeatureInProgress {
		return nil, ErrReadOnly
	}
	if isAgent && g.Status == domain.GateApproved {
		return nil, apperr.Conflict("gate_approved", "the agent does not edit approved gates")
	}
	l, ok, err := s.store.AcquireLock(ctx, f.ID, p.UserID)
	if err != nil {
		return nil, err
	}
	if !ok {
		name := ""
		if l != nil {
			name = l.LockedByName
		}
		return nil, apperr.Locked("feature_locked", "feature is being edited by "+name).With("userName", name)
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	path := specPath(f, area)
	cur, err := s.git.GetFile(ctx, token, f.Branch, path)
	if err != nil && !errors.Is(err, git.ErrNotFound) {
		return nil, auth.MapGitError(err)
	}
	curSHA := ""
	if cur != nil {
		curSHA = git.BlobSHA(cur.Content)
	}
	if in.BaseSHA != "" && in.BaseSHA != curSHA {
		return nil, apperr.Conflict("stale_document", "the document was changed concurrently, reload it").With("currentSha", curSHA)
	}
	newSHA := git.BlobSHA([]byte(in.Content))
	if newSHA == curSHA {
		return &SaveResult{SHA: curSHA}, nil // no change, no commit (no noise in git)
	}
	msg := git.Trailers{Feature: f.UniqueID, Area: string(area), Agent: isAgent}.Message(fmt.Sprintf("%s: edit %s specification", f.UniqueID, area))
	commit, err := s.git.Commit(ctx, token, f.Branch, msg, []git.FileChange{{Path: path, Content: []byte(in.Content)}})
	if errors.Is(err, git.ErrConflict) {
		return nil, apperr.Conflict("stale_document", "the branch changed concurrently, retry")
	}
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	return &SaveResult{SHA: newSHA, Commit: &commit}, nil
}

// DiffResult is the diff since the last approval (or since the default branch).
type DiffResult struct {
	Base       string              `json:"base"`
	BaseLabel  string              `json:"baseLabel"` // "approved" | "default_branch"
	Head       string              `json:"head"`
	ApprovedBy *string             `json:"approvedBy"`
	ApprovedAt *time.Time          `json:"approvedAt"`
	BaseURL    string              `json:"baseUrl"`
	HeadURL    string              `json:"headUrl"`
	Lines      []markdown.DiffLine `json:"lines"`
}

// Diff compares the approved version with the current one. In a domain without
// approval (or before the first approval) the base is the default branch.
func (s *Service) Diff(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*DiffResult, error) {
	f, g, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	path := specPath(f, area)
	ref := s.readRef(f)
	head, err := s.git.LatestCommit(ctx, token, ref, path)
	if err != nil && !errors.Is(err, git.ErrNotFound) {
		return nil, auth.MapGitError(err)
	}
	res := &DiffResult{Head: head, HeadURL: s.git.CommitURL(head)}
	base := s.defaultBranch
	res.BaseLabel = "default_branch"
	if f.ApprovalRequired && g.ApprovedCommit != nil {
		base = *g.ApprovedCommit
		res.BaseLabel = "approved"
		res.ApprovedBy, res.ApprovedAt = g.ApprovedByName, g.ApprovedAt
		res.BaseURL = s.git.CommitURL(base)
	}
	res.Base = base
	oldContent, err := s.content(ctx, token, base, path)
	if err != nil {
		return nil, err
	}
	newContent, err := s.content(ctx, token, ref, path)
	if err != nil {
		return nil, err
	}
	res.Lines = markdown.Diff(oldContent, newContent)
	return res, nil
}

func (s *Service) content(ctx context.Context, token, ref, path string) (string, error) {
	f, err := s.git.GetFile(ctx, token, ref, path)
	if errors.Is(err, git.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", auth.MapGitError(err)
	}
	return string(f.Content), nil
}

// HistoryItem is a gate history entry.
type HistoryItem struct {
	ID        uuid.UUID            `json:"id"`
	Type      domain.GateEventType `json:"type"`
	Actor     *string              `json:"actor"`
	IsAgent   bool                 `json:"isAgent"`
	Commit    *string              `json:"commit"`
	CommitURL *string              `json:"commitUrl"`
	CreatedAt time.Time            `json:"createdAt"`
}

// History lists events of the area, including those of earlier deleted gates.
func (s *Service) History(ctx context.Context, uniqueID string, area domain.Area, page httpx.Page) (httpx.List[HistoryItem], error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return httpx.List[HistoryItem]{}, err
	}
	evs, err := s.store.History(ctx, f.ID, area, page)
	if err != nil {
		return httpx.List[HistoryItem]{}, err
	}
	items := make([]HistoryItem, 0, len(evs))
	for _, e := range evs {
		it := HistoryItem{ID: e.ID, Type: e.Type, Actor: e.ActorName, IsAgent: e.IsAgent, Commit: e.CommitSHA, CreatedAt: e.CreatedAt}
		if e.CommitSHA != nil {
			u := s.git.CommitURL(*e.CommitSHA)
			it.CommitURL = &u
		}
		items = append(items, it)
	}
	return httpx.NewList(items, page.Limit, func(h HistoryItem) (time.Time, string) { return h.CreatedAt, h.ID.String() }), nil
}

// Meta is GET /gates/{area}.
func (s *Service) Meta(ctx context.Context, uniqueID string, area domain.Area) (*specdata.GateDTO, error) {
	_, g, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	d := specdata.ToGateDTO(*g)
	return &d, nil
}

// RecordTransition counts a gate transition metric.
func RecordTransition(area domain.Area, to string) {
	metrics.GateTransitions.WithLabelValues(string(area), to).Inc()
}
