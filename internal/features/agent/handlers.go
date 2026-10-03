package agent

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

func pageOf(limit int) httpx.Page { return httpx.Page{Limit: limit} }

// Routes mounts /chat routes.
func (s *Service) Routes(r chi.Router) {
	r.Post("/chat/messages", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in SendInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		res, err := s.Send(r.Context(), p, in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusAccepted, res)
		return nil
	}))
	// FTR.HMR.CMN-0004: repeat a failed message (ERR-08) and the model that answers (R11).
	r.Post("/chat/messages/{id}/retry", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return err
		}
		res, err := s.Retry(r.Context(), p, id)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusAccepted, res)
		return nil
	}))
	r.Get("/chat/session", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if _, err := httpx.MustPrincipal(r); err != nil {
			return err
		}
		info, err := s.Session(r.Context())
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, info)
		return nil
	}))
	r.Post("/chat/cancel", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		s.Cancel(p.UserID)
		httpx.NoContent(w)
		return nil
	}))
	r.Get("/chat/history", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		page, err := httpx.ParsePage(r)
		if err != nil {
			return err
		}
		msgs, err := s.repo.History(r.Context(), p.UserID, page)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, httpx.NewList(msgs, page.Limit, func(m Message) (time.Time, string) { return m.CreatedAt, m.ID.String() }))
		return nil
	}))
}
