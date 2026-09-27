package features

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// Handlers serves /api/v1/features.
type Handlers struct{ svc *Service }

// NewHandlers creates handlers.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Routes mounts the routes.
func (h *Handlers) Routes(r chi.Router) {
	r.Get("/features", httpx.Handler(h.list))
	r.Post("/features", httpx.Handler(h.create))
	r.Get("/features/{uniqueId}", httpx.Handler(h.get))
	r.Delete("/features/{uniqueId}", httpx.Handler(h.delete))
	r.Post("/features/{uniqueId}/lock", httpx.Handler(h.lock))
	r.Delete("/features/{uniqueId}/lock", httpx.Handler(h.unlock))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && status != "in_progress" && status != "handed_off" && status != "all" {
		return apperr.BadRequest("invalid_status", "status must be in_progress, handed_off or all")
	}
	items, err := h.svc.List(r.Context(), specdata.ListFilter{UserID: p.UserID, Domain: q.Get("domain"), Status: status, Query: q.Get("q"), Page: page})
	if err != nil {
		return err
	}
	out := make([]specdata.FeatureSummary, 0, len(items))
	for _, it := range items {
		out = append(out, specdata.ToSummary(it))
	}
	httpx.JSON(w, 200, httpx.NewList(out, page.Limit, func(f specdata.FeatureSummary) (time.Time, string) {
		return f.CreatedAt, idOf(items, f.UniqueID)
	}))
	return nil
}

func idOf(items []specdata.ListedFeature, uid string) string {
	for _, it := range items {
		if it.UniqueID == uid {
			return it.ID.String()
		}
	}
	return ""
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	var in CreateInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	f, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"uniqueId": f.UniqueID, "prUrl": f.PRURL})
	return nil
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	c, err := h.svc.Get(r.Context(), p, chi.URLParam(r, "uniqueId"))
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, c)
	return nil
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	var in struct {
		ConfirmUniqueID string `json:"confirmUniqueId"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	if err := h.svc.Delete(r.Context(), p, chi.URLParam(r, "uniqueId"), in.ConfirmUniqueID); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handlers) lock(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	l, err := h.svc.Lock(r.Context(), p, chi.URLParam(r, "uniqueId"))
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, specdata.ToLockDTO(l))
	return nil
}

func (h *Handlers) unlock(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	if err := h.svc.Unlock(r.Context(), p, chi.URLParam(r, "uniqueId")); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}
