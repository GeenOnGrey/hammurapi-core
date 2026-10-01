package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// AUTH-07: state-changing requests need a matching X-CSRF-Token.
func TestRequireSessionCSRF(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := RequireSession(ok)
	sess := &httpx.Session{ID: uuid.New(), UserID: uuid.New(), CSRFToken: "tok"}
	cases := []struct {
		name, method, header, cookie string
		want                         int
	}{
		{"get needs no token", http.MethodGet, "", "", 204},
		{"post without token", http.MethodPost, "", "tok", 403},
		{"post with wrong token", http.MethodPost, "bad", "tok", 403},
		{"cookie mismatch", http.MethodPut, "tok", "other", 403},
		{"valid", http.MethodDelete, "tok", "tok", 204},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "/api/v1/x", nil)
		req = req.WithContext(httpx.WithSession(req.Context(), sess))
		if c.header != "" {
			req.Header.Set("X-CSRF-Token", c.header)
		}
		if c.cookie != "" {
			req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: c.cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 401 {
		t.Fatalf("no session: %d", rec.Code)
	}
}
