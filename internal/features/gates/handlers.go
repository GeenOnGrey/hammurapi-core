package gates

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Handlers serves /api/v1/features/{uniqueId}/gates.
type Handlers struct{ svc *Service }

// NewHandlers creates handlers.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Routes mounts the routes.
func (h *Handlers) Routes(r chi.Router) {
	r.Post("/features/{uniqueId}/gates", httpx.Handler(h.add))
	r.Route("/features/{uniqueId}/gates/{area}", func(r chi.Router) {
		r.Get("/", httpx.Handler(h.meta))
		r.Delete("/", httpx.Handler(h.delete))
		r.Get("/document", httpx.Handler(h.getDoc))
		r.Put("/document", httpx.Handler(h.putDoc))
		r.Get("/diff", httpx.Handler(h.diff))
		r.Get("/history", httpx.Handler(h.history))
	})
}

func params(r *http.Request) (*domain.Principal, string, domain.Area, error) {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return nil, "", "", err
	}
	a, err := httpx.ParamArea(r)
	return p, chi.URLParam(r, "uniqueId"), a, err
}

func (h *Handlers) add(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	var in struct {
		Area string `json:"area"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	a, err := domain.ParseArea(in.Area)
	if err != nil {
		return apperr.BadRequest("invalid_area", err.Error())
	}
	g, err := h.svc.AddGate(r.Context(), p, chi.URLParam(r, "uniqueId"), a)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"area": g.Area, "status": g.Status, "headCommit": g.HeadCommit})
	return nil
}

func (h *Handlers) meta(w http.ResponseWriter, r *http.Request) error {
	_, uid, a, err := params(r)
	if err != nil {
		return err
	}
	m, err := h.svc.Meta(r.Context(), uid, a)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, m)
	return nil
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) error {
	p, uid, a, err := params(r)
	if err != nil {
		return err
	}
	if err := h.svc.DeleteGate(r.Context(), p, uid, a); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handlers) getDoc(w http.ResponseWriter, r *http.Request) error {
	p, uid, a, err := params(r)
	if err != nil {
		return err
	}
	d, err := h.svc.GetDocument(r.Context(), p, uid, a)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, d)
	return nil
}

func (h *Handlers) putDoc(w http.ResponseWriter, r *http.Request) error {
	p, uid, a, err := params(r)
	if err != nil {
		return err
	}
	var in SaveInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	res, err := h.svc.SaveDocument(r.Context(), p, uid, a, in, false)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, res)
	return nil
}

func (h *Handlers) diff(w http.ResponseWriter, r *http.Request) error {
	p, uid, a, err := params(r)
	if err != nil {
		return err
	}
	d, err := h.svc.Diff(r.Context(), p, uid, a)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, d)
	return nil
}

func (h *Handlers) history(w http.ResponseWriter, r *http.Request) error {
	_, uid, a, err := params(r)
	if err != nil {
		return err
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	l, err := h.svc.History(r.Context(), uid, a, page)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, l)
	return nil
}
