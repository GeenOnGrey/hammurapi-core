package auth

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/logging"
)

// Cookie names.
const (
	SessionCookie = "hmr_session"
	CSRFCookie    = "csrf_token"
	stateCookie   = "hmr_oauth_state"
)

// PublicConfig is returned by GET /api/v1/config.
type PublicConfig struct {
	Provider        string   `json:"provider"`
	UploadMaxBytes  int64    `json:"uploadMaxBytes"`
	UploadTypes     []string `json:"uploadAllowedTypes"`
	ImportMaxBytes  int64    `json:"importMaxBytes"`
	Languages       []string `json:"languages"`
	DefaultLanguage string   `json:"defaultLanguage"`
	DefaultBranch   string   `json:"defaultBranch"`
}

// Handlers serves auth endpoints.
type Handlers struct {
	svc    *Service
	cfg    PublicConfig
	secure bool
}

// NewHandlers creates handlers; secure sets the Secure cookie attribute.
func NewHandlers(svc *Service, cfg PublicConfig, secure bool) *Handlers {
	return &Handlers{svc: svc, cfg: cfg, secure: secure}
}

// Public mounts unauthenticated routes.
func (h *Handlers) Public(r chi.Router) {
	r.Get("/config", func(w http.ResponseWriter, _ *http.Request) { httpx.JSON(w, 200, h.cfg) })
	r.Get("/auth/login", h.login)
	r.Get("/auth/callback", h.callback)
}

// Private mounts routes that need a session.
func (h *Handlers) Private(r chi.Router) {
	r.Get("/auth/me", httpx.Handler(h.me))
	r.Post("/auth/logout", httpx.Handler(h.logout))
}

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/api/v1/auth", HttpOnly: true,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, h.svc.provider.AuthCodeURL(state, h.svc.RedirectURL()), http.StatusFound)
}

func (h *Handlers) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	q := r.URL.Query()
	if err != nil || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(q.Get("state"))) != 1 {
		http.Redirect(w, r, "/login?error=state", http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/api/v1/auth", MaxAge: -1})
	if q.Get("error") != "" {
		http.Redirect(w, r, "/login?error=denied", http.StatusFound)
		return
	}
	sid, csrf, err := h.svc.Login(r.Context(), q.Get("code"))
	if err != nil {
		slog.ErrorContext(r.Context(), "login failed", "err", err)
		http.Redirect(w, r, "/login?error=failed", http.StatusFound)
		return
	}
	h.setSessionCookies(w, sid, csrf)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handlers) setSessionCookies(w http.ResponseWriter, sid uuid.UUID, csrf string) {
	maxAge := int(SessionTTL.Seconds())
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: sid.String(), Path: "/", HttpOnly: true,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
	http.SetCookie(w, &http.Cookie{Name: CSRFCookie, Value: csrf, Path: "/", HttpOnly: false,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

// Me is the response of /auth/me.
type Me struct {
	ID          uuid.UUID   `json:"id"`
	Username    string      `json:"username"`
	DisplayName string      `json:"displayName"`
	AvatarURL   *string     `json:"avatarUrl"`
	GlobalAdmin bool        `json:"globalAdmin"`
	Roles       []RoleAreas `json:"roles"`
	Language    string      `json:"language"`
	Theme       string      `json:"theme"`
	AgentName   string      `json:"agentName"`
	AgentTone   string      `json:"agentTone"`
}

// RoleAreas is a role with its areas.
type RoleAreas struct {
	Role  domain.Role   `json:"role"`
	Areas []domain.Area `json:"areas"`
}

// RolesOf lists roles of a principal in a stable order.
func RolesOf(p *domain.Principal) []RoleAreas {
	out := []RoleAreas{}
	for _, role := range domain.Roles {
		if areas := p.AreasFor(role); len(areas) > 0 {
			out = append(out, RoleAreas{Role: role, Areas: areas})
		}
	}
	return out
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	u, err := h.svc.repo.User(r.Context(), p.UserID)
	if err != nil {
		return err
	}
	if u == nil {
		return apperr.ErrNoSession
	}
	httpx.JSON(w, 200, Me{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, AvatarURL: u.AvatarURL,
		GlobalAdmin: p.GlobalAdmin, Roles: RolesOf(p), Language: u.Language, Theme: u.Theme,
		AgentName: u.AgentName, AgentTone: string(u.AgentTone)})
	return nil
}

func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) error {
	if s := httpx.SessionFrom(r.Context()); s != nil {
		if err := h.svc.repo.DeleteSession(r.Context(), s.ID); err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: CSRFCookie, Path: "/", MaxAge: -1})
	httpx.NoContent(w)
	return nil
}

// Authenticate loads the session and principal when a session cookie is
// present. It never rejects; RequireSession does.
func (h *Handlers) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(SessionCookie)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		sid, err := uuid.Parse(c.Value)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		sess, err := h.svc.repo.Session(ctx, sid)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if sess == nil {
			next.ServeHTTP(w, r)
			return
		}
		p, err := h.svc.repo.Principal(ctx, sess.UserID)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if p == nil {
			next.ServeHTTP(w, r)
			return
		}
		if time.Since(sess.LastSeenAt) > 5*time.Minute {
			_ = h.svc.repo.TouchSession(ctx, sess.ID, SessionTTL)
		}
		httpx.SetUserForLog(r, p.UserID.String())
		ctx = httpx.WithSession(ctx, &httpx.Session{ID: sess.ID, UserID: sess.UserID, CSRFToken: sess.CSRFToken})
		ctx = httpx.WithPrincipal(ctx, p)
		ctx = logging.With(ctx, slog.String("user_id", p.UserID.String()))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireSession rejects requests without a session and enforces the CSRF
// double-submit token on state-changing methods.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := httpx.SessionFrom(r.Context())
		if s == nil {
			httpx.Error(w, r, apperr.ErrNoSession)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if !validCSRF(r, s.CSRFToken) {
				httpx.Error(w, r, apperr.ErrCSRF)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func validCSRF(r *http.Request, sessionToken string) bool {
	hdr := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	c, err := r.Cookie(CSRFCookie)
	if hdr == "" || err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hdr), []byte(c.Value)) == 1 &&
		subtle.ConstantTimeCompare([]byte(hdr), []byte(sessionToken)) == 1
}
