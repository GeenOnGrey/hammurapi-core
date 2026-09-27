package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/oauth2"

	"github.com/GeenOnGrey/hammurapi-core/internal/platform/metrics"
)

// apiClient performs authenticated JSON calls against a provider REST API.
type apiClient struct {
	provider string
	baseAPI  string
	http     *http.Client
	// authHeader builds the Authorization header value for a token.
	authHeader func(token string) (string, string)
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}
}

// call sends a request and decodes the JSON response into out (if non-nil).
// op labels the metrics on failure.
func (c *apiClient) call(ctx context.Context, op, token, method, path string, body, out any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.baseAPI + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		k, v := c.authHeader(token)
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.fail(op)
		return nil, fmt.Errorf("%s %s: %w", c.provider, op, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		c.fail(op)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return resp, ErrNotFound
		case http.StatusUnauthorized:
			return resp, ErrUnauthorized
		}
		return resp, &APIError{Provider: c.provider, Status: resp.StatusCode, Message: errorMessage(raw, resp.Status)}
	}
	metrics.GitProviderUp.Set(1)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp, fmt.Errorf("%s %s: decode: %w", c.provider, op, err)
		}
	}
	return resp, nil
}

func (c *apiClient) fail(op string) {
	metrics.GitAPIErrors.WithLabelValues(c.provider, op).Inc()
}

// errorMessage extracts a message from typical GitHub/GitLab error bodies.
func errorMessage(raw []byte, fallback string) string {
	var v struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &v) == nil {
		parts := []string{}
		switch m := v.Message.(type) {
		case string:
			parts = append(parts, m)
		case []any:
			for _, x := range m {
				parts = append(parts, fmt.Sprint(x))
			}
		case map[string]any:
			for k, x := range m {
				parts = append(parts, fmt.Sprintf("%s: %v", k, x))
			}
		}
		if v.Error != "" {
			parts = append(parts, v.Error)
		}
		if v.Desc != "" {
			parts = append(parts, v.Desc)
		}
		for _, e := range v.Errors {
			if e.Message != "" {
				parts = append(parts, e.Message)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
	}
	if s := strings.TrimSpace(string(raw)); s != "" && len(s) < 300 {
		return s
	}
	return fallback
}

// oauthToken converts an oauth2 token.
func oauthToken(t *oauth2.Token) *Token {
	exp := t.Expiry
	if exp.IsZero() {
		// Non-expiring tokens (classic OAuth apps): treat as long-lived.
		exp = time.Now().Add(365 * 24 * time.Hour)
	}
	return &Token{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Expiry: exp}
}

func exchange(ctx context.Context, cfg oauth2.Config, code, redirectURL string) (*Token, error) {
	cfg.RedirectURL = redirectURL
	ctx = context.WithValue(ctx, oauth2.HTTPClient, newHTTPClient())
	t, err := cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	return oauthToken(t), nil
}

func refresh(ctx context.Context, cfg oauth2.Config, rt string) (*Token, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, newHTTPClient())
	src := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: rt, Expiry: time.Unix(1, 0)})
	t, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: refresh: %v", ErrUnauthorized, err)
	}
	return oauthToken(t), nil
}

func authCodeURL(cfg oauth2.Config, state, redirectURL string) string {
	cfg.RedirectURL = redirectURL
	return cfg.AuthCodeURL(state)
}

func pathEscapeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
