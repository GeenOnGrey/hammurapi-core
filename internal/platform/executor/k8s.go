package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// K8sConfig configures Job creation.
type K8sConfig struct {
	Namespace      string
	Image          string
	Timeout        time.Duration
	CPU, Memory    string   // limits, e.g. "2", "4Gi"
	Env            []string // agent settings only (ACP_*), KEY=VALUE
	EnvFromSecret  string   // Secret with agent credentials (e.g. ANTHROPIC_API_KEY), optional
	APIServer      string   // defaults to https://kubernetes.default.svc
	TokenFile      string   // defaults to the in-cluster service account token
	CAFile         string
	ServiceAccount string // optional; empty — automountServiceAccountToken: false
}

// K8s creates a Job per task.
type K8s struct {
	cfg  K8sConfig
	http *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount/"

// NewK8s creates a Kubernetes executor using in-cluster credentials.
func NewK8s(cfg K8sConfig) (*K8s, error) {
	if cfg.APIServer == "" {
		cfg.APIServer = "https://kubernetes.default.svc"
	}
	if cfg.TokenFile == "" {
		cfg.TokenFile = saDir + "token"
	}
	if cfg.CAFile == "" {
		cfg.CAFile = saDir + "ca.crt"
	}
	if cfg.CPU == "" {
		cfg.CPU = "2"
	}
	if cfg.Memory == "" {
		cfg.Memory = "4Gi"
	}
	pool := x509.NewCertPool()
	if ca, err := os.ReadFile(cfg.CAFile); err == nil {
		pool.AppendCertsFromPEM(ca)
	}
	return &K8s{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}}, nil
}

// Name implements Executor.
func (k *K8s) Name() string { return "k8s" }

// JobName is the Job name of a task.
func JobName(taskID string) string {
	id := strings.ReplaceAll(taskID, "-", "")
	if len(id) > 20 {
		id = id[:20]
	}
	return "hammurapi-task-" + id
}

// Manifest builds the Job object (exported for tests and Helm docs).
func (k *K8s) Manifest(t Task) map[string]any {
	env := []map[string]any{
		{"name": "HAMMURAPI_TASK_ID", "value": t.ID},
		{"name": "HAMMURAPI_TASK_TOKEN", "value": t.Token},
		{"name": "HAMMURAPI_INTERNAL_URL", "value": t.InternalURL},
		{"name": "HAMMURAPI_WORKDIR", "value": "/work"},
		{"name": "TRACEPARENT", "value": t.TraceParent},
		{"name": "HOME", "value": "/work"},
	}
	for _, kv := range k.cfg.Env {
		if n, v, ok := strings.Cut(kv, "="); ok {
			env = append(env, map[string]any{"name": n, "value": v})
		}
	}
	container := map[string]any{
		"name": "runner", "image": k.cfg.Image,
		"args":       []string{"runner", "--task", t.ID},
		"env":        env,
		"workingDir": "/work",
		"resources": map[string]any{
			"limits":   map[string]string{"cpu": k.cfg.CPU, "memory": k.cfg.Memory},
			"requests": map[string]string{"cpu": "250m", "memory": "512Mi"},
		},
		"securityContext": map[string]any{
			"runAsNonRoot": true, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false,
			"capabilities": map[string]any{"drop": []string{"ALL"}},
		},
		"volumeMounts": []map[string]any{{"name": "work", "mountPath": "/work"}, {"name": "tmp", "mountPath": "/tmp"}},
	}
	if k.cfg.EnvFromSecret != "" {
		container["envFrom"] = []map[string]any{{"secretRef": map[string]any{"name": k.cfg.EnvFromSecret}}}
	}
	pod := map[string]any{
		"restartPolicy":                "Never",
		"automountServiceAccountToken": false,
		"containers":                   []any{container},
		"volumes": []map[string]any{
			{"name": "work", "emptyDir": map[string]any{}},
			{"name": "tmp", "emptyDir": map[string]any{}},
		},
	}
	if k.cfg.ServiceAccount != "" {
		pod["serviceAccountName"] = k.cfg.ServiceAccount
	}
	labels := map[string]string{"app.kubernetes.io/name": "hammurapi-runner", "hammurapi/task": t.ID}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": JobName(t.ID), "namespace": k.cfg.Namespace, "labels": labels},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 3600,
			"activeDeadlineSeconds":   int(k.cfg.Timeout.Seconds()),
			"template":                map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod},
		},
	}
}

// Start implements Executor.
func (k *K8s) Start(ctx context.Context, t Task) (string, error) {
	body, _ := json.Marshal(k.Manifest(t))
	path := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", k.cfg.Namespace)
	status, raw, err := k.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	if status == http.StatusConflict {
		return JobName(t.ID), nil // already created (idempotent effect retry)
	}
	if status >= 300 {
		return "", fmt.Errorf("create job: %d %s", status, trim(raw))
	}
	return JobName(t.ID), nil
}

// Stop implements Executor.
func (k *K8s) Stop(ctx context.Context, ref string) error {
	path := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s?propagationPolicy=Background", k.cfg.Namespace, ref)
	status, raw, err := k.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status >= 300 {
		return fmt.Errorf("delete job: %d %s", status, trim(raw))
	}
	return nil
}

// Status implements Executor.
func (k *K8s) Status(ctx context.Context, ref string) (Status, error) {
	path := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s", k.cfg.Namespace, ref)
	status, raw, err := k.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return StatusUnknown, err
	}
	if status == http.StatusNotFound {
		return StatusUnknown, nil
	}
	var job struct {
		Status struct {
			Active    int `json:"active"`
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return StatusUnknown, err
	}
	switch {
	case job.Status.Succeeded > 0:
		return StatusFinished, nil
	case job.Status.Failed > 0:
		return StatusFailed, nil
	}
	return StatusRunning, nil
}

func (k *K8s) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.cfg.APIServer+path, r)
	if err != nil {
		return 0, nil, err
	}
	token, err := os.ReadFile(k.cfg.TokenFile)
	if err != nil {
		return 0, nil, fmt.Errorf("service account token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
