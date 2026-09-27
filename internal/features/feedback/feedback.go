// Package feedback turns user feedback into an issue in the instance repository.
package feedback

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Routes mounts POST /feedback.
func Routes(r chi.Router, provider git.Provider, tokens git.TokenSource) {
	r.Post("/feedback", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			Text string `json:"text"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		text := strings.TrimSpace(in.Text)
		if text == "" || len(text) > 20000 {
			return apperr.Unprocessable("invalid_text", "feedback text is required")
		}
		token, err := tokens.Token(r.Context(), p.UserID)
		if err != nil {
			return err
		}
		title := firstLine(text)
		body := fmt.Sprintf("%s\n\n---\nSent from Hammurapi by @%s", text, p.Username)
		url, err := provider.CreateIssue(r.Context(), token, "Hammurapi feedback: "+title, body)
		if err != nil {
			return auth.MapGitError(err)
		}
		httpx.JSON(w, http.StatusCreated, map[string]string{"url": url})
		return nil
	}))
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	if r := []rune(l); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return l
}
