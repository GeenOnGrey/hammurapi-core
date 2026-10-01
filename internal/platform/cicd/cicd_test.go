package cicd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/signing"
)

func TestRenderEscapes(t *testing.T) {
	got := Render(map[string]string{"svc": "{service}", "cb": "{callback_url}?run={run_id}"},
		Vars{Service: "booking\n; rm -rf /", CallbackURL: "https://h/hooks/v1/deploy", RunID: "r1"}, map[string]string{"run_e2e": "true"})
	if got["svc"] != "booking; rm -rf /" || got["cb"] != "https://h/hooks/v1/deploy?run=r1" || got["run_e2e"] != "true" {
		t.Fatalf("got %v", got)
	}
}

func TestApplyOverride(t *testing.T) {
	s := Settings{Workflow: "deploy.yml", Params: map[string]string{"a": "1", "b": "2"}}
	o := s.Apply(&Override{Workflow: "custom.yml", Params: map[string]string{"b": "3"}})
	if o.Workflow != "custom.yml" || o.Params["a"] != "1" || o.Params["b"] != "3" || s.Params["b"] != "2" {
		t.Fatalf("got %+v", o)
	}
}

func TestWebhookSigned(t *testing.T) {
	var ok bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		ok = signing.Verify(r.Header, body, []string{"s3cret"}, time.Now()) && m["runId"] == "run-1"
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	tr := New(nil)
	_, err := tr.Start(context.Background(), Request{
		Settings: Settings{Type: "webhook", URL: srv.URL, Auth: "secret"},
		Vars:     Vars{Service: "booking", RunID: "run-1", Environment: "stage"},
		Secret:   "s3cret",
	})
	if err != nil || !ok {
		t.Fatalf("err=%v verified=%v", err, ok)
	}
}
