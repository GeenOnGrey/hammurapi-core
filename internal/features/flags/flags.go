// Package flags receives feature flag events (FTR.HMR.CMN-0002 R29): the universal
// signed webhook POST /hooks/v1/feature-flags with a published contract, and
// the rotation of its secret. Only production events change release steps;
// Hammurapi never switches flags itself.
package flags

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/releases"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/signing"
)

// Payload is the webhook body.
type Payload struct {
	Flag        string    `json:"flag"`
	State       string    `json:"state"` // on | off
	Environment string    `json:"environment"`
	ChangedAt   time.Time `json:"changedAt"`
	Actor       string    `json:"actor"`
}

// Hook serves POST /hooks/v1/feature-flags.
type Hook struct {
	Pool    *pgxpool.Pool
	Secrets *signing.Secrets
	Events  events.Publisher
	Now     func() time.Time
}

func (h *Hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	var set cycledata.FlagsSetting
	if _, err := cycledata.New(h.Pool).Setting(ctx, "feature_flags", &set); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if !signing.Verify(r.Header, body, h.Secrets.ResolveAll(set.SecretRefs), now()) {
		metrics.WebhookEvents.WithLabelValues("unauthorized").Inc()
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil || p.Flag == "" || (p.State != "on" && p.State != "off") || p.Environment == "" {
		http.Error(w, "flag, state (on|off) and environment are required", http.StatusBadRequest)
		return
	}
	if p.ChangedAt.IsZero() {
		p.ChangedAt = now()
	}
	if err := postgres.InTx(ctx, h.Pool, func(tx pgx.Tx) error {
		return releases.RecordFlag(ctx, tx, p.Flag, p.State, p.Environment, p.ChangedAt, p.Actor, "webhook")
	}); err != nil {
		slog.ErrorContext(ctx, "feature flag webhook", "err", err)
		http.Error(w, "internal error", http.StatusServiceUnavailable)
		return
	}
	if p.Environment == "production" {
		h.Events.Publish(ctx, events.Event{Type: events.ReleaseUpdated, Data: map[string]any{"flag": p.Flag, "state": p.State}})
	}
	metrics.WebhookEvents.WithLabelValues("accepted").Inc()
	w.WriteHeader(http.StatusAccepted)
}

// AdminRoutes mounts POST /admin/api/v1/settings/feature-flags/secret/rotate.
func (h *Hook) AdminRoutes(r chi.Router) {
	r.Post("/settings/feature-flags/secret/rotate", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if !p.GlobalAdmin {
			return apperr.Forbidden("forbidden", "global administrator role required")
		}
		cd := cycledata.New(h.Pool)
		var set cycledata.FlagsSetting
		if _, err := cd.Setting(r.Context(), "feature_flags", &set); err != nil {
			return err
		}
		v, ref, err := h.Secrets.Generate()
		if err != nil {
			return err
		}
		set.SecretRefs = signing.Rotate(set.SecretRefs, ref)
		if err := cd.PutSetting(r.Context(), "feature_flags", set, &p.UserID); err != nil {
			return err
		}
		httpx.JSON(w, 200, map[string]any{"secret": v, "activeSecrets": len(set.SecretRefs)})
		return nil
	}))
}
