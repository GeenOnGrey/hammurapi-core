// Package webhooks receives git provider push events (api) and projects them
// onto gates (worker). Git is the source of truth for document content: any
// commit touching a gate folder that Hammurapi does not already know about is an
// edit, and an edit returns the gate to draft.
package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/kafka"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// Envelope is the raw webhook as published to Kafka.
type Envelope struct {
	Provider string            `json:"provider"`
	Headers  map[string]string `json:"headers"`
	Body     json.RawMessage   `json:"body"`
}

var forwardedHeaders = []string{"X-GitHub-Event", "X-GitHub-Delivery", "X-Gitlab-Event", "X-Gitlab-Event-UUID"}

// Receiver handles POST /hooks/v1/git.
type Receiver struct {
	provider git.Provider
	secret   string
	bus      kafka.Publisher
}

// NewReceiver creates the receiver.
func NewReceiver(provider git.Provider, secret string, bus kafka.Publisher) *Receiver {
	return &Receiver{provider: provider, secret: secret, bus: bus}
}

func (h *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		metrics.WebhookEvents.WithLabelValues("bad_request").Inc()
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if !h.provider.VerifyWebhook(r.Header, body, h.secret) {
		metrics.WebhookEvents.WithLabelValues("unauthorized").Inc()
		http.Error(w, "invalid webhook secret", http.StatusUnauthorized)
		return
	}
	if !json.Valid(body) {
		metrics.WebhookEvents.WithLabelValues("bad_request").Inc()
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var ref struct {
		Ref string `json:"ref"`
	}
	_ = json.Unmarshal(body, &ref)
	branch := strings.TrimPrefix(ref.Ref, "refs/heads/")
	key := branch
	if uid, ok := git.UniqueIDFromBranch(branch); ok {
		key = uid
	}
	env := Envelope{Provider: h.provider.Name(), Headers: map[string]string{}, Body: body}
	for _, k := range forwardedHeaders {
		if v := r.Header.Get(k); v != "" {
			env.Headers[k] = v
		}
	}
	msg, _ := json.Marshal(env)
	if err := h.bus.Publish(r.Context(), kafka.TopicGitPush, key, msg); err != nil {
		slog.ErrorContext(r.Context(), "publish webhook", "err", err)
		metrics.WebhookEvents.WithLabelValues("error").Inc()
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "queue_unavailable", "message": "try again later"}})
		return
	}
	metrics.WebhookEvents.WithLabelValues("accepted").Inc()
	w.WriteHeader(http.StatusAccepted)
}

// Processor applies push events to the projection (worker).
type Processor struct {
	store    specdata.Store
	provider git.Provider
	events   events.Publisher
}

// NewProcessor creates the processor.
func NewProcessor(store specdata.Store, provider git.Provider, ev events.Publisher) *Processor {
	return &Processor{store: store, provider: provider, events: ev}
}

// Handle is the Kafka handler.
func (p *Processor) Handle(ctx context.Context, _, value []byte) error {
	var env Envelope
	if err := json.Unmarshal(value, &env); err != nil {
		slog.ErrorContext(ctx, "drop malformed webhook envelope", "err", err)
		return nil
	}
	h := http.Header{}
	for k, v := range env.Headers {
		h.Set(k, v)
	}
	ev, ok, err := p.provider.ParsePush(h, env.Body)
	if err != nil {
		slog.ErrorContext(ctx, "drop unparsable push event", "err", err)
		return nil
	}
	if !ok {
		return nil // not a push event
	}
	return p.Apply(ctx, ev)
}

type pending struct {
	uniqueID         string
	gate             specdata.Gate
	approvalsChanged bool
}

// Apply processes one push event idempotently.
func (p *Processor) Apply(ctx context.Context, ev *git.PushEvent) error {
	uid, ok := git.UniqueIDFromBranch(ev.Branch)
	if !ok {
		// Pushes to the default branch or rules/* branches need no projection:
		// rules are read from git on demand.
		return nil
	}
	var published []pending
	err := p.store.InTx(ctx, func(tx specdata.Store) error {
		published = nil
		fresh, err := tx.MarkWebhookProcessed(ctx, ev.EventID)
		if err != nil {
			return err
		}
		if !fresh {
			slog.InfoContext(ctx, "duplicate webhook skipped", "event", ev.EventID)
			return nil
		}
		f, err := tx.FeatureByUniqueID(ctx, uid)
		if errors.Is(err, specdata.ErrNotFound) {
			slog.InfoContext(ctx, "push to unknown feature branch ignored", "branch", ev.Branch)
			return nil
		}
		if err != nil {
			return err
		}
		if f.Status != domain.FeatureInProgress {
			return nil // deleted or handed-off features are not projected
		}
		actor, err := tx.UserIDByUsername(ctx, ev.Actor)
		if err != nil {
			return err
		}
		for _, c := range ev.Commits {
			out, err := p.applyCommit(ctx, tx, f, c, actor)
			if err != nil {
				return err
			}
			published = append(published, out...)
		}
		return nil
	})
	if err != nil {
		return err
	}
	metrics.WebhookEvents.WithLabelValues("processed").Inc()
	approvals := false
	for _, x := range published {
		g := x.gate
		gates.PublishGateUpdated(ctx, p.events, x.uniqueID, &g)
		approvals = approvals || x.approvalsChanged
	}
	if approvals {
		p.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{"uniqueId": uid}})
	}
	return nil
}

func (p *Processor) applyCommit(ctx context.Context, tx specdata.Store, f *specdata.Feature, c git.PushCommit, actor *uuid.UUID) ([]pending, error) {
	trailers := git.ParseTrailers(c.Message)
	type touch struct{ specRemoved bool }
	areas := map[domain.Area]*touch{}
	visit := func(path string, removed bool) {
		loc, ok := git.ParseSpecPath(path)
		if !ok {
			if !strings.HasPrefix(path, "rules/") {
				slog.InfoContext(ctx, "path outside specs/<d>/<s>/<id>/<area>/ ignored", "path", path, "commit", c.SHA)
			}
			return
		}
		if loc.Domain != f.DomainKey || loc.System != f.SystemKey || loc.UniqueID != f.UniqueID {
			slog.InfoContext(ctx, "path of another feature ignored", "path", path, "feature", f.UniqueID)
			return
		}
		a := domain.Area(loc.Area)
		if !a.Valid() {
			slog.InfoContext(ctx, "unknown area ignored", "path", path)
			return
		}
		t := areas[a]
		if t == nil {
			t = &touch{}
			areas[a] = t
		}
		if removed && loc.Rest == "spec.md" {
			t.specRemoved = true
		}
	}
	for _, x := range c.Added {
		visit(x, false)
	}
	for _, x := range c.Modified {
		visit(x, false)
	}
	for _, x := range c.Removed {
		visit(x, true)
	}
	var out []pending
	sha := c.SHA
	for _, area := range domain.Areas {
		t, ok := areas[area]
		if !ok {
			continue
		}
		g, err := tx.ActiveGate(ctx, f.ID, area)
		if errors.Is(err, specdata.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if g.HeadCommit == sha {
			continue // a commit Hammurapi made itself (creation, import, deletion)
		}
		wasInReview := g.Status == domain.GateInReview
		if t.specRemoved {
			active, err := tx.ActiveGates(ctx, f.ID)
			if err != nil {
				return nil, err
			}
			if len(active) <= 1 {
				slog.WarnContext(ctx, "last gate's spec.md removed directly in git; feature kept", "feature", f.UniqueID, "area", area)
				continue
			}
			now := time.Now()
			g.DeletedAt, g.DeletedBy, g.HeadCommit = &now, actor, sha
			if err := tx.SaveGate(ctx, g); err != nil {
				return nil, err
			}
			if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventDeleted, ActorID: actor, IsAgent: trailers.Agent, CommitSHA: &sha}); err != nil {
				return nil, err
			}
			out = append(out, pending{uniqueID: f.UniqueID, gate: *g, approvalsChanged: wasInReview})
			continue
		}
		g.HeadCommit = sha
		if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventEdited, ActorID: actor, IsAgent: trailers.Agent, CommitSHA: &sha}); err != nil {
			return nil, err
		}
		if g.Status != domain.GateDraft {
			g.Status, g.SubmittedAt = domain.GateDraft, nil
			if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventReset, ActorID: actor, IsAgent: trailers.Agent, CommitSHA: &sha}); err != nil {
				return nil, err
			}
			gates.RecordTransition(area, string(domain.GateDraft))
		}
		if err := tx.SaveGate(ctx, g); err != nil {
			return nil, err
		}
		out = append(out, pending{uniqueID: f.UniqueID, gate: *g, approvalsChanged: wasInReview})
	}
	return out, nil
}
