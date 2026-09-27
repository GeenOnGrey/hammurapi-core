package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
)

// SSEHandler serves GET /api/v1/events: one stream carries every event type.
func SSEHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := httpx.PrincipalFrom(r.Context())
		if p == nil {
			httpx.Error(w, r, apperr.ErrNoSession)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "retry: 3000\n: connected\n\n")
		fl.Flush()

		sub := hub.Subscribe(p.UserID)
		defer hub.Unsubscribe(sub)
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			case e := <-sub.C:
				data, err := json.Marshal(e.Data)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
				fl.Flush()
			}
		}
	}
}
