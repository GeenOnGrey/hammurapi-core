// Package cicd starts deploy pipelines (HMR.CMN-0002 arch §12). Hammurapi does
// not deploy itself: it starts a pipeline and waits for the signed result
// webhook POST /hooks/v1/deploy.
//
// Types: github-actions (workflow_dispatch), gitlab-ci (pipeline API) — both
// through the git provider with the bot token — and webhook (any URL from the
// admin settings, signed request or secret header).
package cicd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/signing"
)

// Settings of one environment (deploy_settings row).
type Settings struct {
	Type           string            `json:"type"` // github-actions | gitlab-ci | webhook
	Workflow       string            `json:"workflow,omitempty"`
	Ref            string            `json:"ref,omitempty"`
	URL            string            `json:"url,omitempty"`
	Auth           string            `json:"auth"` // bot | secret
	Params         map[string]string `json:"params"`
	TimeoutMinutes int               `json:"timeoutMinutes"`
}

// Override is a per-service override (catalog annotation or admin panel).
type Override struct {
	Workflow string            `json:"workflow,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
}

// Apply returns settings with the override applied.
func (s Settings) Apply(o *Override) Settings {
	if o == nil {
		return s
	}
	if o.Workflow != "" {
		s.Workflow = o.Workflow
	}
	if o.Ref != "" {
		s.Ref = o.Ref
	}
	if len(o.Params) > 0 {
		p := map[string]string{}
		for k, v := range s.Params {
			p[k] = v
		}
		for k, v := range o.Params {
			p[k] = v
		}
		s.Params = p
	}
	return s
}

// Vars are the template variables of the parameters.
type Vars struct {
	Service     string
	Repo        string
	Ref         string
	Environment string
	Feature     string
	Release     string
	CallbackURL string
	RunID       string
}

func (v Vars) replacer() *strings.Replacer {
	return strings.NewReplacer(
		"{service}", v.Service, "{repo}", v.Repo, "{ref}", v.Ref, "{environment}", v.Environment,
		"{feature}", v.Feature, "{release}", v.Release, "{callback_url}", v.CallbackURL, "{run_id}", v.RunID)
}

// Render substitutes variables in the parameter template. Values are plain
// strings sent as JSON (never concatenated into a shell or URL), which is the
// escaping: a value cannot break out of its field. Control characters are dropped.
func Render(tpl map[string]string, v Vars, extra map[string]string) map[string]string {
	r := v.replacer()
	out := map[string]string{}
	for k, val := range tpl {
		out[k] = clean(r.Replace(val))
	}
	for k, val := range extra {
		out[k] = clean(val)
	}
	return out
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// Trigger starts deploy pipelines.
type Trigger struct {
	git  git.Provider
	http *http.Client
	now  func() time.Time
}

// New creates a trigger. provider is the instance git provider (bound to any repo).
func New(provider git.Provider) *Trigger {
	return &Trigger{git: provider, http: &http.Client{Timeout: 30 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}, now: time.Now}
}

// Request is one pipeline start.
type Request struct {
	Settings Settings
	Vars     Vars
	Extra    map[string]string // e.g. run_e2e, dryRun
	Secret   string            // for webhook auth=secret, and signing
}

// Start starts the pipeline and returns a run URL if the CI system reports one.
func (t *Trigger) Start(ctx context.Context, req Request) (string, error) {
	s := req.Settings
	params := Render(s.Params, req.Vars, req.Extra)
	ref := s.Ref
	if ref == "" {
		ref = req.Vars.Ref
	} else {
		ref = req.Vars.replacer().Replace(ref)
	}
	switch s.Type {
	case "github-actions", "gitlab-ci":
		if t.git == nil {
			return "", errors.New("git provider is not configured")
		}
		if s.Type == "github-actions" && t.git.Name() != "github" || s.Type == "gitlab-ci" && t.git.Name() != "gitlab" {
			return "", fmt.Errorf("deploy type %s does not match the git provider %s", s.Type, t.git.Name())
		}
		p := t.git.ForRepo(req.Vars.Repo)
		token, err := p.BotToken(ctx)
		if err != nil {
			return "", fmt.Errorf("bot token: %w", err)
		}
		return p.RunPipeline(ctx, token, s.Workflow, ref, params)
	case "webhook":
		return "", t.webhook(ctx, s, req, params, ref)
	}
	return "", fmt.Errorf("unknown deploy type %q", s.Type)
}

func (t *Trigger) webhook(ctx context.Context, s Settings, req Request, params map[string]string, ref string) error {
	if s.URL == "" {
		return errors.New("deploy webhook URL is not set")
	}
	body, _ := json.Marshal(map[string]any{
		"runId": req.Vars.RunID, "service": req.Vars.Service, "repo": req.Vars.Repo, "ref": ref,
		"environment": req.Vars.Environment, "feature": req.Vars.Feature, "release": req.Vars.Release,
		"callbackUrl": req.Vars.CallbackURL, "params": params,
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	if req.Secret != "" {
		signing.SetHeaders(r.Header, req.Secret, body, t.now())
		if s.Auth == "secret" {
			r.Header.Set("Authorization", "Bearer "+req.Secret)
		}
	}
	resp, err := t.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("deploy webhook: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
