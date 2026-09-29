package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestK8sManifestIsolation(t *testing.T) {
	k, _ := NewK8s(K8sConfig{Namespace: "hammurapi-runners", Image: "img", Timeout: 2 * time.Hour, Env: []string{"ACP_AGENT_COMMAND=claude-agent-acp"}})
	m := k.Manifest(Task{ID: "0b6c1f7e-1111-2222-3333-444455556666", Token: "tok", InternalURL: "http://api:8081"})
	raw, _ := json.Marshal(m)
	s := string(raw)
	for _, want := range []string{`"automountServiceAccountToken":false`, `"readOnlyRootFilesystem":true`, `"runAsNonRoot":true`,
		`"activeDeadlineSeconds":7200`, `"ttlSecondsAfterFinished":3600`, `"backoffLimit":0`, `"ACP_AGENT_COMMAND"`} {
		if !strings.Contains(s, want) {
			t.Errorf("manifest lacks %s", want)
		}
	}
	if strings.Contains(s, "DATABASE_URL") {
		t.Error("instance secrets leaked into the job")
	}
}

func TestK8sStartIdempotent(t *testing.T) {
	calls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			w.WriteHeader(401)
			return
		}
		if calls > 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600)
	k, _ := NewK8s(K8sConfig{Namespace: "ns", Image: "img", Timeout: time.Hour, APIServer: srv.URL, TokenFile: tokenFile})
	k.http = srv.Client()
	for i := 0; i < 2; i++ {
		ref, err := k.Start(context.Background(), Task{ID: "task-1"})
		if err != nil || ref != JobName("task-1") {
			t.Fatalf("start %d: %v %s", i, err, ref)
		}
	}
}

func TestMinimalEnvHasNoSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	for _, kv := range minimalEnv() {
		if strings.HasPrefix(kv, "DATABASE_URL=") {
			t.Fatal("DATABASE_URL inherited")
		}
	}
}
