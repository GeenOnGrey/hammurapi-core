package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// HMR.INFRA-0002: the SPA on web.<domain>, the API on api.<domain>.
func TestCallbackRedirectsToWeb(t *testing.T) {
	h := NewHandlers(nil, PublicConfig{}, true).WithWeb("https://web.example.org/", "example.org")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?state=s1&error=access_denied", nil)
	req.AddCookie(&http.Cookie{Name: stateCookie, Value: "s1"})
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	if got := rec.Header().Get("Location"); got != "https://web.example.org/login?error=denied" {
		t.Fatalf("Location = %q", got)
	}
}

func TestCallbackRelativeWithoutWeb(t *testing.T) {
	h := NewHandlers(nil, PublicConfig{}, false)
	rec := httptest.NewRecorder()
	h.callback(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback", nil))
	if got := rec.Header().Get("Location"); got != "/login?error=state" {
		t.Fatalf("Location = %q", got)
	}
}

func TestSessionCookiesUseSharedDomain(t *testing.T) {
	h := NewHandlers(nil, PublicConfig{}, true).WithWeb("https://web.example.org", "example.org")
	rec := httptest.NewRecorder()
	h.setSessionCookies(rec, uuid.New(), "csrf")
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("cookies: %v", cookies)
	}
	for _, c := range cookies {
		if c.Domain != "example.org" || !c.Secure || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("%s: domain=%q secure=%v samesite=%v", c.Name, c.Domain, c.Secure, c.SameSite)
		}
	}
}
