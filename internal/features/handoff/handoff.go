// Package handoff implements "hand off to implementation": merging the
// feature's PR/MR into the default branch.
package handoff

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// Service implements the hand-off.
type Service struct {
	store  specdata.Store
	git    git.Provider
	tokens git.TokenSource
	events events.Publisher
}

// NewService creates the service.
func NewService(store specdata.Store, provider git.Provider, tokens git.TokenSource, ev events.Publisher) *Service {
	return &Service{store: store, git: provider, tokens: tokens, events: ev}
}

// Handoff merges the PR/MR. It needs every gate approved, unless approval is
// disabled for the domain (read at action time), in which case the hand-off
// is recorded as done without approval.
func (s *Service) Handoff(ctx context.Context, p *domain.Principal, uniqueID string) error {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return err
	}
	if f.Status != domain.FeatureInProgress {
		return apperr.Conflict("feature_handed_off", "the feature was already handed off")
	}
	gs, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return err
	}
	if !specdata.HasAnyRoleIn(p, gs) {
		return apperr.Forbidden("forbidden", "editor or approver role in one of the feature's areas required")
	}
	if f.ApprovalRequired && !specdata.AllApproved(gs) {
		return apperr.Conflict("not_all_approved", "every gate must be approved before hand-off")
	}
	if err := features.LockedByOther(ctx, s.store, f.ID, p.UserID); err != nil {
		return err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return err
	}
	if err := s.git.MergePR(ctx, token, f.PRNumber, fmt.Sprintf("%s %s: hand off", f.UniqueID, f.Title)); err != nil {
		return auth.MapGitError(err)
	}
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := tx.MarkHandedOff(ctx, f.ID, p.UserID, !f.ApprovalRequired); err != nil {
			return err
		}
		return tx.DropLock(ctx, f.ID)
	})
	if err != nil {
		return err
	}
	s.events.Publish(ctx, events.Event{Type: events.FeatureHandedOff, Data: map[string]any{"uniqueId": f.UniqueID, "withoutApproval": !f.ApprovalRequired}})
	return nil
}

// Routes mounts POST /features/{uniqueId}/handoff.
func (s *Service) Routes(r chi.Router) {
	r.Post("/features/{uniqueId}/handoff", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := s.Handoff(r.Context(), p, chi.URLParam(r, "uniqueId")); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
}
