package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A busy operator (503 agent_busy) makes the task wait for a free session
// instead of failing it; other refusals fail at once.
func TestOpenSessionWaitsWhileBusy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"agent_busy","message":"busy","details":{"retryAfter":1}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"agentUrl":"http://agent:8090","sessionId":"s1","sessionToken":"t","model":"m"}`))
	}))
	defer srv.Close()
	c := &client{cfg: Config{InternalURL: srv.URL, Token: "task"}, http: srv.Client()}
	waited := 0
	var s AgentSession
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := openSession(ctx, c, "t1", map[string]any{}, &s, func() { waited++ }); err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "s1" || waited != 1 || calls.Load() != 2 {
		t.Fatalf("session %+v, waited %d, calls %d", s, waited, calls.Load())
	}
}

func TestOpenSessionOtherErrorsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"resume_exhausted","message":"no"}}`))
	}))
	defer srv.Close()
	c := &client{cfg: Config{InternalURL: srv.URL, Token: "task"}, http: srv.Client()}
	var s AgentSession
	err := openSession(context.Background(), c, "t1", map[string]any{}, &s, func() { t.Fatal("must not wait") })
	var ce *callError
	if !errors.As(err, &ce) || ce.Code != "resume_exhausted" || ce.Status != http.StatusConflict {
		t.Fatalf("%v", err)
	}
}
