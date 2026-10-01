// Package agent implements the chat with the user's agent: messages over ACP,
// token streaming over SSE, and the MCP tools the agent works through.
package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agentrun"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/attachments"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/acp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// historyOnRestore is how many recent messages seed a session that could not be loaded.
const historyOnRestore = 20

var toneGuide = map[domain.AgentTone]string{
	domain.ToneBusiness: "business-like: polite, precise, no small talk",
	domain.ToneFriendly: "friendly and warm, but still to the point",
	domain.ToneConcise:  "concise: shortest correct answer, no filler",
	domain.ToneMentor:   "mentor: explain the reasoning and suggest what to consider next",
}

// ChatContext is the object the chat works on (PLT.HMR-0002 tech §7).
type ChatContext struct {
	Type string  `json:"type"` // issue | feature | release
	Key  string  `json:"key"`
	Area *string `json:"area"`
}

// SendInput is the body of POST /chat/messages.
type SendInput struct {
	Text          string       `json:"text"`
	Mode          string       `json:"mode"`
	Context       *ChatContext `json:"context"`
	AttachmentIDs []uuid.UUID  `json:"attachmentIds"`
	IsVoice       bool         `json:"isVoice"`
}

// resolved is a loaded chat context.
type resolved struct {
	Type, Key, Title, Domain string
	Phase                    string
	Area                     *domain.Area
	Feature                  *specdata.Feature
}

// PrincipalLoader loads a user's current roles.
type PrincipalLoader func(ctx context.Context, userID uuid.UUID) (*domain.Principal, error)

// Service implements the chat.
type Service struct {
	repo        *Repository
	store       specdata.Store
	agent       acp.AgentClient
	mcp         *mcp.Server
	mcpURL      string
	hub         events.Publisher // local delivery: the ACP session and SSE stream share the pod
	attachments *attachments.Service
	principal   PrincipalLoader

	mu      sync.Mutex
	tokens  map[uuid.UUID]string             // MCP token per user session
	running map[uuid.UUID]context.CancelFunc // one running prompt per user
	lastCtx map[uuid.UUID]string             // last context block sent
}

// NewService creates the service.
func NewService(repo *Repository, store specdata.Store, agent acp.AgentClient, m *mcp.Server, mcpURL string,
	hub events.Publisher, att *attachments.Service, principal PrincipalLoader) *Service {
	return &Service{repo: repo, store: store, agent: agent, mcp: m, mcpURL: mcpURL, hub: hub, attachments: att, principal: principal,
		tokens: map[uuid.UUID]string{}, running: map[uuid.UUID]context.CancelFunc{}, lastCtx: map[uuid.UUID]string{}}
}

// OnSessionClosed revokes the MCP token of a dropped ACP session.
func (s *Service) OnSessionClosed(userID uuid.UUID) {
	s.mu.Lock()
	tok := s.tokens[userID]
	delete(s.tokens, userID)
	delete(s.lastCtx, userID)
	s.mu.Unlock()
	if tok != "" {
		s.mcp.Revoke(tok)
	}
}

// ResetPersona closes the session so the next one gets the new name and tone.
func (s *Service) ResetPersona(userID uuid.UUID) {
	s.agent.CloseSession(userID)
	_ = s.repo.ForgetSession(context.Background(), userID)
	s.OnSessionClosed(userID)
}

// Accepted is the response of POST /chat/messages.
type Accepted struct {
	MessageID uuid.UUID `json:"messageId"`
	CreatedAt time.Time `json:"createdAt"`
}

// Send stores the user's message and asks the agent; the answer streams over SSE.
func (s *Service) Send(ctx context.Context, p *domain.Principal, in SendInput) (*Accepted, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, apperr.Unprocessable("empty_message", "message text is required")
	}
	if in.Mode != "general" && in.Mode != "spec" {
		return nil, apperr.Unprocessable("invalid_mode", "mode must be general or spec")
	}
	var rc *resolved
	if in.Mode == "spec" {
		if in.Context == nil || in.Context.Key == "" {
			return nil, apperr.Unprocessable("context_required", "context is required in spec mode")
		}
		var err error
		if rc, err = s.resolve(ctx, *in.Context); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if _, busy := s.running[p.UserID]; busy {
		s.mu.Unlock()
		return nil, apperr.Conflict("agent_busy", "the agent is still answering the previous message")
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.running[p.UserID] = cancel
	s.mu.Unlock()

	var msgID uuid.UUID
	var at time.Time
	err := s.repo.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if msgID, at, err = s.repo.Insert(ctx, tx, p.UserID, "user", in.Mode, rc, text, in.IsVoice); err != nil {
			return err
		}
		return s.attachments.Link(ctx, tx, p.UserID, msgID, in.AttachmentIDs)
	})
	if err != nil {
		s.finish(p.UserID)
		return nil, err
	}
	go s.run(runCtx, p, in, rc, msgID, text)
	return &Accepted{MessageID: msgID, CreatedAt: at}, nil
}

func (s *Service) finish(userID uuid.UUID) {
	s.mu.Lock()
	if c, ok := s.running[userID]; ok {
		c()
		delete(s.running, userID)
	}
	s.mu.Unlock()
}

// Cancel stops the running answer.
func (s *Service) Cancel(userID uuid.UUID) {
	s.agent.Cancel(userID)
}

func (s *Service) emit(ctx context.Context, userID uuid.UUID, typ string, data any) {
	uid := userID
	s.hub.Publish(ctx, events.Event{Type: typ, UserID: &uid, Data: data})
}

// resolve loads the chat context: an issue, a feature or a release.
func (s *Service) resolve(ctx context.Context, c ChatContext) (*resolved, error) {
	rc := &resolved{Type: c.Type}
	if c.Area != nil && *c.Area != "" {
		a, err := domain.ParseArea(*c.Area)
		if err != nil {
			return nil, apperr.BadRequest("invalid_area", err.Error())
		}
		rc.Area = &a
	}
	cd := cycledata.New(s.store.Q())
	switch c.Type {
	case "issue":
		is, _, err := cd.IssueByKey(ctx, c.Key)
		if errors.Is(err, cycledata.ErrNotFound) {
			return nil, apperr.NotFound("issue_not_found", "issue not found")
		}
		if err != nil {
			return nil, err
		}
		rc.Key, rc.Title, rc.Domain, rc.Phase = is.Key, is.Title, is.Domain, string(is.Status)
	case "feature":
		f, err := features.Load(ctx, s.store, c.Key)
		if err != nil {
			return nil, err
		}
		rc.Key, rc.Title, rc.Domain, rc.Phase, rc.Feature = f.UniqueID, f.Title, f.DomainKey, string(f.Phase), f
	case "release":
		r, err := cd.ReleaseByKey(ctx, c.Key)
		if errors.Is(err, cycledata.ErrNotFound) {
			return nil, apperr.NotFound("release_not_found", "release not found")
		}
		if err != nil {
			return nil, err
		}
		rc.Key, rc.Title, rc.Domain, rc.Phase = r.Key, r.FeatureTitle, r.Domain, r.Status
	default:
		return nil, apperr.Unprocessable("invalid_context", "context type must be issue, feature or release")
	}
	return rc, nil
}

func (s *Service) grant(p *domain.Principal, mode string, rc *resolved) mcp.Grant {
	g := mcp.Grant{UserID: p.UserID, Mode: mode}
	if rc != nil {
		g.ContextType, g.ContextKey, g.Expert = rc.Type, rc.Key, p.IsExpertOf(rc.Domain)
		if rc.Feature != nil {
			g.Feature = rc.Feature.UniqueID
		}
		if rc.Area != nil {
			g.Area = *rc.Area
		}
	}
	return g
}

func (s *Service) run(ctx context.Context, p *domain.Principal, in SendInput, rc *resolved, msgID uuid.UUID, text string) {
	defer s.finish(p.UserID)
	uid := p.UserID
	fail := func(code, msg string, err error) {
		if err != nil {
			slog.ErrorContext(ctx, "agent request failed", "user_id", uid, "code", code, "err", err)
		}
		s.emit(ctx, uid, events.AgentError, map[string]string{"messageId": msgID.String(), "code": code, "message": msg})
	}
	persona, err := s.repo.Persona(ctx, uid)
	if err != nil {
		fail("internal", "internal error", err)
		return
	}
	grant := s.grant(p, in.Mode, rc)
	s.mu.Lock()
	tok := s.tokens[uid]
	s.mu.Unlock()
	if tok != "" {
		s.mcp.Update(tok, grant) // mode or feature switch changes the token's rights
	}
	prev, _ := s.repo.SessionID(ctx, uid)
	info, err := s.agent.Ensure(ctx, uid, func(caps acp.AgentCaps) acp.SessionSetup {
		t := s.mcp.Issue(grant)
		s.mu.Lock()
		old := s.tokens[uid]
		s.tokens[uid] = t
		delete(s.lastCtx, uid)
		s.mu.Unlock()
		if old != "" {
			s.mcp.Revoke(old)
		}
		return acp.SessionSetup{
			MCPServers:        []acp.MCPServer{s.mcpServer(caps, t)},
			Meta:              map[string]any{"hammurapi": map[string]any{"agentName": persona.Name, "tone": persona.Tone, "language": persona.Language}},
			PreviousSessionID: prev,
		}
	})
	if err != nil {
		if errors.Is(err, acp.ErrNotConfigured) {
			fail("agent_not_configured", "the agent is not configured on this instance", err)
		} else {
			fail("agent_unavailable", "the agent is unavailable", err)
		}
		return
	}
	if info.Fresh {
		_ = s.repo.SaveSessionID(ctx, uid, info.ID)
	}

	var blocks []acp.ContentBlock
	ctxText := contextBlock(persona, in.Mode, rc, grant)
	s.mu.Lock()
	changed := s.lastCtx[uid] != ctxText
	s.lastCtx[uid] = ctxText
	s.mu.Unlock()
	if info.Fresh && !info.Loaded {
		if h := s.historyBlock(ctx, uid, msgID); h != "" {
			blocks = append(blocks, acp.TextBlock(h))
		}
	}
	if changed || info.Fresh {
		blocks = append(blocks, acp.TextBlock(ctxText))
	}
	blocks = append(blocks, s.attachmentBlocks(ctx, uid, in.AttachmentIDs, caps(s.agent, info))...)
	blocks = append(blocks, acp.TextBlock(text))

	var answer strings.Builder
	var mu sync.Mutex
	stop, err := s.agent.Prompt(ctx, uid, blocks, func(u acp.Update) {
		switch u.Kind {
		case "token":
			mu.Lock()
			answer.WriteString(u.Text)
			mu.Unlock()
			s.emit(ctx, uid, events.AgentToken, map[string]string{"messageId": msgID.String(), "text": u.Text})
		case "tool_call", "tool_call_update":
			s.emit(ctx, uid, events.AgentToolCall, map[string]string{"messageId": msgID.String(), "toolCallId": u.ToolCallID,
				"title": u.Title, "status": u.Status, "kind": u.Kind})
		case "error":
			s.emit(ctx, uid, events.AgentError, map[string]string{"messageId": msgID.String(), "code": "agent_crashed", "message": u.Text})
		}
	})
	if err != nil {
		if errors.Is(err, acp.ErrClosed) {
			_ = s.repo.ForgetSession(ctx, uid)
			fail("agent_crashed", "the agent stopped unexpectedly; the next message starts a new session", err)
		} else if errors.Is(err, context.Canceled) {
			fail("cancelled", "cancelled", nil)
		} else {
			fail("agent_failed", "the agent could not answer", err)
		}
		return
	}
	mu.Lock()
	content := answer.String()
	mu.Unlock()
	if strings.TrimSpace(content) == "" {
		content = "…"
	}
	agentMsgID, at, err := s.repo.Insert(context.WithoutCancel(ctx), s.repo.pool, uid, "agent", in.Mode, rc, content, false)
	if err != nil {
		fail("internal", "internal error", err)
		return
	}
	s.emit(ctx, uid, events.AgentDone, map[string]any{"messageId": msgID.String(), "agentMessageId": agentMsgID.String(),
		"stopReason": stop, "content": truncate(content, 6000), "createdAt": at})
}

// caps is a helper for prompt capabilities; Pool does not expose them per
// session, so image blocks are only sent to agents that declared support.
func caps(a acp.AgentClient, _ acp.SessionInfo) acp.AgentCaps {
	if c, ok := a.(interface{ Caps() acp.AgentCaps }); ok {
		return c.Caps()
	}
	return acp.AgentCaps{}
}

func (s *Service) mcpServer(caps acp.AgentCaps, token string) acp.MCPServer {
	return agentrun.MCPServerFor(caps, s.mcpURL, token)
}

func contextBlock(p Persona, mode string, rc *resolved, g mcp.Grant) string {
	var b strings.Builder
	b.WriteString("[Hammurapi context — not from the user]\n")
	fmt.Fprintf(&b, "You are %s, the user's partner in Hammurapi — the platform where teams and AI agents go through research, development and release around the specification. ", p.Name)
	fmt.Fprintf(&b, "Communication tone: %s. The tone affects only how you talk, never the content or style of drafts.\n", toneGuide[p.Tone])
	fmt.Fprintf(&b, "Interface language of the user: %s (answer in the language the user writes in).\n", p.Language)
	if mode == "general" || rc == nil {
		b.WriteString("Mode: GENERAL QUESTIONS — questions across all issues, features, releases and specifications. Do not edit documents in this mode; if the user asks for an edit, suggest opening the issue or feature and switching the chat mode.\n")
		b.WriteString("You cannot delete anything; the user does that in the interface.")
		return b.String()
	}
	switch rc.Type {
	case "issue":
		fmt.Fprintf(&b, "Context: ISSUE %s \"%s\" (domain %s, status %s). ", rc.Key, rc.Title, rc.Domain, rc.Phase)
		if g.CanEditDiscovery() {
			b.WriteString("You may update its Discovery document with edit_discovery: it must answer what value the issue has and how to measure that the goal is reached (metric source, query, target, window). After your update the issue returns to verification.\n")
		} else {
			b.WriteString("The user is not an expert of this domain, so you cannot change the Discovery document.\n")
		}
	case "feature":
		fmt.Fprintf(&b, "Context: FEATURE %s \"%s\" (%s, phase %s).", rc.Key, rc.Title, rc.Domain, rc.Phase)
		if rc.Area != nil {
			fmt.Fprintf(&b, " Open area: %s.", *rc.Area)
		}
		switch {
		case rc.Phase != string(domain.PhaseSpec):
			b.WriteString(" The specification is read-only in this phase.\n")
		case !g.Expert:
			b.WriteString(" The user is not an expert of this domain, so you cannot edit documents.\n")
		default:
			b.WriteString(" You may edit the product, design and arch gates with edit_spec, only while not approved; use read_rules before drafting a gate from scratch. " +
				"The tech and qa gates are generated: never edit them — if the user comments on them, call regenerate_gate with the comment. " +
				"Product requirements need IDs in the form **R<n>.** with acceptance criteria.\n")
		}
	case "release":
		fmt.Fprintf(&b, "Context: RELEASE %s of \"%s\" (%s, step %s). Answer questions about the release; you have no tools to change it — merges, deploys, confirmation and rollback are done by the expert in the interface.\n", rc.Key, rc.Title, rc.Domain, rc.Phase)
	}
	b.WriteString("You cannot delete anything; the user does that in the interface.")
	return b.String()
}

func (s *Service) historyBlock(ctx context.Context, userID, current uuid.UUID) string {
	msgs, err := s.repo.History(ctx, userID, pageOf(historyOnRestore+1))
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var lines []string
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.ID == current {
			continue
		}
		who := "User"
		if m.Role == "agent" {
			who = "You"
		}
		lines = append(lines, fmt.Sprintf("%s: %s", who, truncate(m.Content, 2000)))
	}
	if len(lines) == 0 {
		return ""
	}
	return "[Hammurapi — previous conversation, restored after the session moved]\n" + strings.Join(lines, "\n")
}

func (s *Service) attachmentBlocks(ctx context.Context, userID uuid.UUID, ids []uuid.UUID, c acp.AgentCaps) []acp.ContentBlock {
	var out []acp.ContentBlock
	for _, id := range ids {
		a, data, err := s.attachments.Read(ctx, userID, id)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(a.MimeType, "image/") && c.Image:
			out = append(out, acp.ContentBlock{Type: "image", MimeType: a.MimeType, Data: base64.StdEncoding.EncodeToString(data)})
		case strings.HasPrefix(a.MimeType, "text/") && c.Embedded:
			out = append(out, acp.ContentBlock{Type: "resource", Resource: &acp.EmbeddedResource{
				URI: "hammurapi://attachments/" + a.ID.String() + "/" + a.FileName, Text: string(data), MimeType: a.MimeType}})
		case strings.HasPrefix(a.MimeType, "text/"):
			out = append(out, acp.TextBlock(fmt.Sprintf("[Attached file %s]\n%s", a.FileName, truncate(string(data), 50000))))
		default:
			out = append(out, acp.TextBlock(fmt.Sprintf("[The user attached %s (%s, %d bytes); its content is not available to you.]", a.FileName, a.MimeType, a.SizeBytes)))
		}
	}
	return out
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
