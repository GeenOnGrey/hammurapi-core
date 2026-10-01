// Package auth implements sign-in through the instance's git provider,
// browser sessions with CSRF protection, and user token management.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

// SessionTTL is the sliding lifetime of a browser session.
const SessionTTL = 30 * 24 * time.Hour

// ErrReauth means the user's provider token cannot be refreshed; sign in again.
var ErrReauth = apperr.Unauthorized("reauth_required", "git provider session expired, sign in again")

var agentNames = []string{"Codex", "Stele", "Nabu", "Scribe", "Tablet", "Enki", "Lamassu", "Ziggurat", "Marduk", "Cuneus"}

// Service handles sign-in and tokens.
type Service struct {
	repo            *Repository
	provider        git.Provider
	box             *crypto.Box
	publicURL       string
	bootstrapAdmins map[string]bool
	defaultLanguage string

	refreshMu sync.Map // per-user refresh serialization
}

// NewService creates the service.
func NewService(repo *Repository, provider git.Provider, box *crypto.Box, publicURL string, bootstrapAdmins []string, defaultLanguage string) *Service {
	ba := map[string]bool{}
	for _, a := range bootstrapAdmins {
		ba[strings.ToLower(a)] = true
	}
	if !domain.ValidLanguage(defaultLanguage) {
		defaultLanguage = "en"
	}
	return &Service{repo: repo, provider: provider, box: box, publicURL: publicURL, bootstrapAdmins: ba, defaultLanguage: defaultLanguage}
}

// RedirectURL is the OAuth callback URL registered at the provider.
func (s *Service) RedirectURL() string { return s.publicURL + "/api/v1/auth/callback" }

// Login completes the OAuth flow: exchanges the code, upserts the user and
// creates a session. It returns session id and CSRF token.
func (s *Service) Login(ctx context.Context, code string) (uuid.UUID, string, error) {
	tok, err := s.provider.Exchange(ctx, code, s.RedirectURL())
	if err != nil {
		return uuid.Nil, "", apperr.Unauthorized("oauth_failed", err.Error())
	}
	pu, err := s.provider.CurrentUser(ctx, tok.AccessToken)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("load provider user: %w", err)
	}
	u, created, err := s.repo.UpsertUser(ctx, UserRow{
		ProviderUID: pu.ID, Username: pu.Username, DisplayName: pu.Name, AvatarURL: nilIfEmpty(pu.AvatarURL),
		Language:    s.defaultLanguage,
		AgentName:   randomPick(agentNames),
		AgentTone:   randomPick(domain.Tones),
		GlobalAdmin: s.bootstrapAdmins[strings.ToLower(pu.Username)],
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	if created {
		slog.InfoContext(ctx, "user created", "user_id", u.ID, "username", u.Username, "global_admin", u.GlobalAdmin)
	}
	if err := s.storeToken(ctx, u.ID, tok); err != nil {
		return uuid.Nil, "", err
	}
	csrf := randomHex(32)
	sid, err := s.repo.CreateSession(ctx, u.ID, csrf, SessionTTL)
	if err != nil {
		return uuid.Nil, "", err
	}
	return sid, csrf, nil
}

func (s *Service) storeToken(ctx context.Context, userID uuid.UUID, t *git.Token) error {
	acc, err := s.box.Seal(t.AccessToken)
	if err != nil {
		return err
	}
	var ref []byte
	if t.RefreshToken != "" {
		if ref, err = s.box.Seal(t.RefreshToken); err != nil {
			return err
		}
	}
	return s.repo.SaveToken(ctx, userID, EncryptedToken{Access: acc, Refresh: ref, ExpiresAt: t.Expiry})
}

// Token implements git.TokenSource: returns a valid access token, refreshing it
// with the refresh token when it is about to expire.
func (s *Service) Token(ctx context.Context, userID uuid.UUID) (string, error) {
	mu, _ := s.refreshMu.LoadOrStore(userID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	et, err := s.repo.Token(ctx, userID)
	if err != nil {
		return "", err
	}
	if et == nil {
		return "", ErrReauth
	}
	if time.Until(et.ExpiresAt) > 2*time.Minute {
		return s.box.Open(et.Access)
	}
	if len(et.Refresh) == 0 {
		return "", ErrReauth
	}
	rt, err := s.box.Open(et.Refresh)
	if err != nil {
		return "", err
	}
	nt, err := s.provider.Refresh(ctx, rt)
	if err != nil {
		slog.WarnContext(ctx, "token refresh failed", "user_id", userID, "err", err)
		if errors.Is(err, git.ErrUnauthorized) {
			_ = s.repo.DeleteToken(ctx, userID)
			return "", ErrReauth
		}
		return "", err
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = rt
	}
	if err := s.storeToken(ctx, userID, nt); err != nil {
		return "", err
	}
	return nt.AccessToken, nil
}

// MapGitError converts provider errors of user-initiated calls to API errors.
func MapGitError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, git.ErrUnauthorized):
		return ErrReauth
	}
	var ae *git.APIError
	if errors.As(err, &ae) {
		return apperr.Unprocessable("provider_refused", ae.Message).With("providerStatus", ae.Status)
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomPick[T any](xs []T) T {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(xs))))
	return xs[n.Int64()]
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
