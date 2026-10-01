// Package acp is the Agent Client Protocol client. Hammurapi runs the agent as
// a subprocess of the api pod and talks JSON-RPC over stdio. Work with the agent
// is hidden behind AgentClient, so the transport can change later.
package acp

//go:generate go tool mockgen -destination=mocks/agent.go -package=mocks . AgentClient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/telemetry"
)

// ProtocolVersion is the ACP major version spoken by this client.
const ProtocolVersion = 1

// ContentBlock is an ACP prompt content block.
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is a text resource embedded in a prompt.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	Text     string `json:"text"`
	MimeType string `json:"mimeType,omitempty"`
}

// TextBlock builds a text content block.
func TextBlock(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

// NameValue is an ACP env/header pair.
type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer describes an MCP server passed in session/new: either HTTP (Type="http")
// or stdio (Command set).
type MCPServer struct {
	Type    string      `json:"type,omitempty"`
	Name    string      `json:"name"`
	URL     string      `json:"url,omitempty"`
	Headers []NameValue `json:"headers,omitempty"`
	Command string      `json:"command,omitempty"`
	Args    []string    `json:"args,omitempty"`
	Env     []NameValue `json:"env,omitempty"`
}

// MarshalJSON emits the stdio shape without type, and the http shape with headers.
func (s MCPServer) MarshalJSON() ([]byte, error) {
	if s.Type == "http" || s.Type == "sse" {
		h := s.Headers
		if h == nil {
			h = []NameValue{}
		}
		return json.Marshal(map[string]any{"type": s.Type, "name": s.Name, "url": s.URL, "headers": h})
	}
	args, env := s.Args, s.Env
	if args == nil {
		args = []string{}
	}
	if env == nil {
		env = []NameValue{}
	}
	return json.Marshal(map[string]any{"name": s.Name, "command": s.Command, "args": args, "env": env})
}

// AgentCaps are the capabilities the agent declared in initialize.
type AgentCaps struct {
	LoadSession bool
	HTTPMCP     bool
	Image       bool
	Embedded    bool
}

// SessionSetup is what the caller provides when a session has to be (re)created.
type SessionSetup struct {
	MCPServers        []MCPServer
	Meta              map[string]any
	PreviousSessionID string // try session/load with this id when supported
}

// SessionInfo describes the user's current session.
type SessionInfo struct {
	ID string
	// Fresh is true when the session was created by this call.
	Fresh bool
	// Loaded is true when a fresh session was restored with session/load.
	Loaded bool
}

// Update is a streamed agent update.
type Update struct {
	Kind       string `json:"kind"` // token | tool_call | tool_call_update
	Text       string `json:"text,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	Title      string `json:"title,omitempty"`
	Status     string `json:"status,omitempty"`
}

// AgentClient is the abstraction over the agent transport.
type AgentClient interface {
	// Ensure returns the user's session, creating it (or restoring it) with setup when needed.
	Ensure(ctx context.Context, userID uuid.UUID, setup func(AgentCaps) SessionSetup) (SessionInfo, error)
	// Prompt sends a prompt to the user's session, streaming updates, and returns the stop reason.
	Prompt(ctx context.Context, userID uuid.UUID, blocks []ContentBlock, onUpdate func(Update)) (string, error)
	// Cancel cancels the running prompt of the user.
	Cancel(userID uuid.UUID)
	// CloseSession drops the user's session (e.g. after a tone change).
	CloseSession(userID uuid.UUID)
}

// Config configures the process pool.
type Config struct {
	Command     string
	Args        []string
	Env         []string
	MaxProcs    int
	IdleTimeout time.Duration
	// OnSessionClosed is invoked when a session is dropped (idle, crash, explicit close).
	OnSessionClosed func(userID uuid.UUID)
}

// ErrNotConfigured means ACP_AGENT_COMMAND is empty.
var ErrNotConfigured = errors.New("agent is not configured (ACP_AGENT_COMMAND)")

type process struct {
	cmd      *exec.Cmd
	conn     *conn
	caps     AgentCaps
	sessions int
	dead     bool
	lastUsed time.Time
}

type session struct {
	userID   uuid.UUID
	id       string
	proc     *process
	lastUsed time.Time
	sink     func(Update)
	mu       sync.Mutex // one prompt at a time per user
}

// Pool implements AgentClient with a bounded pool of agent processes.
type Pool struct {
	cfg      Config
	mu       sync.Mutex
	procs    []*process
	sessions map[uuid.UUID]*session
	byID     map[string]*session
	stop     chan struct{}
}

// NewPool creates the pool and starts the idle janitor.
func NewPool(cfg Config) *Pool {
	if cfg.MaxProcs <= 0 {
		cfg.MaxProcs = 1
	}
	p := &Pool{cfg: cfg, sessions: map[uuid.UUID]*session{}, byID: map[string]*session{}, stop: make(chan struct{})}
	go p.janitor()
	return p
}

// Close kills all agent processes.
func (p *Pool) Close() {
	close(p.stop)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.procs {
		pr.kill()
	}
}

func (pr *process) kill() {
	pr.dead = true
	if pr.cmd != nil && pr.cmd.Process != nil {
		_ = pr.cmd.Process.Kill()
	}
}

// startProcess launches an agent and runs initialize. Called with p.mu held.
func (p *Pool) startProcess(ctx context.Context) (*process, error) {
	if p.cfg.Command == "" {
		return nil, ErrNotConfigured
	}
	cmd := exec.Command(p.cfg.Command, p.cfg.Args...)
	cmd.Env = append(os.Environ(), p.cfg.Env...)
	cmd.Stderr = &logWriter{}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	pr := &process{cmd: cmd, lastUsed: time.Now()}
	pr.conn = newConn(stdout, stdin, p.onNotify, p.onRequest)
	go func() {
		err := cmd.Wait()
		pr.conn.close(fmt.Errorf("agent exited: %v", err))
		p.onProcessExit(pr, err)
	}()

	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var res struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image           bool `json:"image"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	// fs and terminal are deliberately not declared: the agent works only through MCP tools.
	err = pr.conn.call(ictx, "initialize", map[string]any{
		"protocolVersion":    ProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "hammurapi", "version": "1.0.0"},
	}, &res)
	if err != nil {
		pr.kill()
		return nil, fmt.Errorf("agent initialize: %w", err)
	}
	ac := res.AgentCapabilities
	pr.caps = AgentCaps{LoadSession: ac.LoadSession, HTTPMCP: ac.MCPCapabilities.HTTP, Image: ac.PromptCapabilities.Image, Embedded: ac.PromptCapabilities.EmbeddedContext}
	p.procs = append(p.procs, pr)
	metrics.AgentProcesses.Set(float64(len(p.procs)))
	return pr, nil
}

// pickProcess returns the process for a new session: spawn a new one while under
// the limit and every running process is busy, otherwise the least loaded one.
func (p *Pool) pickProcess(ctx context.Context) (*process, error) {
	var best *process
	for _, pr := range p.procs {
		if pr.dead {
			continue
		}
		if best == nil || pr.sessions < best.sessions {
			best = pr
		}
	}
	alive := 0
	for _, pr := range p.procs {
		if !pr.dead {
			alive++
		}
	}
	if best == nil || (best.sessions > 0 && alive < p.cfg.MaxProcs) {
		return p.startProcess(ctx)
	}
	return best, nil
}

func (p *Pool) onProcessExit(pr *process, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr.dead = true
	slog.Warn("agent process exited", "err", err)
	for i, x := range p.procs {
		if x == pr {
			p.procs = append(p.procs[:i], p.procs[i+1:]...)
			break
		}
	}
	for uid, s := range p.sessions {
		if s.proc == pr {
			if s.sink != nil {
				s.sink(Update{Kind: "error", Text: "agent process exited"})
			}
			p.dropLocked(uid, s)
		}
	}
	metrics.AgentProcesses.Set(float64(len(p.procs)))
}

func (p *Pool) dropLocked(uid uuid.UUID, s *session) {
	delete(p.sessions, uid)
	delete(p.byID, s.id)
	if s.proc != nil {
		s.proc.sessions--
	}
	metrics.AgentSessions.Set(float64(len(p.sessions)))
	if p.cfg.OnSessionClosed != nil {
		go p.cfg.OnSessionClosed(uid)
	}
}

// Ensure implements AgentClient.
func (p *Pool) Ensure(ctx context.Context, userID uuid.UUID, setup func(AgentCaps) SessionSetup) (SessionInfo, error) {
	ctx, span := telemetry.Start(ctx, "acp.ensure_session")
	defer span.End()
	p.mu.Lock()
	if s, ok := p.sessions[userID]; ok && !s.proc.dead {
		s.lastUsed = time.Now()
		p.mu.Unlock()
		return SessionInfo{ID: s.id}, nil
	}
	pr, err := p.pickProcess(ctx)
	if err != nil {
		p.mu.Unlock()
		return SessionInfo{}, err
	}
	pr.sessions++ // reserve before releasing the lock
	p.mu.Unlock()

	st := setup(pr.caps)
	cwd := os.TempDir()
	info := SessionInfo{Fresh: true}
	if st.PreviousSessionID != "" && pr.caps.LoadSession {
		// Register the id first: load replays history through session/update.
		err := pr.conn.call(ctx, "session/load", map[string]any{
			"sessionId": st.PreviousSessionID, "cwd": cwd, "mcpServers": st.MCPServers, "_meta": st.Meta,
		}, nil)
		if err == nil {
			info.ID, info.Loaded = st.PreviousSessionID, true
		}
	}
	if info.ID == "" {
		var res struct {
			SessionID string `json:"sessionId"`
		}
		if err := pr.conn.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": st.MCPServers, "_meta": st.Meta}, &res); err != nil {
			p.mu.Lock()
			pr.sessions--
			p.mu.Unlock()
			return SessionInfo{}, fmt.Errorf("session/new: %w", err)
		}
		info.ID = res.SessionID
	}
	p.mu.Lock()
	s := &session{userID: userID, id: info.ID, proc: pr, lastUsed: time.Now()}
	p.sessions[userID] = s
	p.byID[info.ID] = s
	metrics.AgentSessions.Set(float64(len(p.sessions)))
	p.mu.Unlock()
	return info, nil
}

// Prompt implements AgentClient.
func (p *Pool) Prompt(ctx context.Context, userID uuid.UUID, blocks []ContentBlock, onUpdate func(Update)) (string, error) {
	ctx, span := telemetry.Start(ctx, "acp.prompt")
	defer span.End()
	p.mu.Lock()
	s, ok := p.sessions[userID]
	p.mu.Unlock()
	if !ok {
		return "", errors.New("acp: no session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p.mu.Lock()
	s.sink = onUpdate
	s.lastUsed = time.Now()
	s.proc.lastUsed = time.Now()
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		s.sink = nil
		s.lastUsed = time.Now()
		p.mu.Unlock()
	}()
	var res struct {
		StopReason string `json:"stopReason"`
	}
	if err := s.proc.conn.call(ctx, "session/prompt", map[string]any{"sessionId": s.id, "prompt": blocks}, &res); err != nil {
		return "", err
	}
	return res.StopReason, nil
}

// Cancel implements AgentClient.
func (p *Pool) Cancel(userID uuid.UUID) {
	p.mu.Lock()
	s, ok := p.sessions[userID]
	p.mu.Unlock()
	if ok {
		_ = s.proc.conn.notify("session/cancel", map[string]string{"sessionId": s.id})
	}
}

// CloseSession implements AgentClient.
func (p *Pool) CloseSession(userID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[userID]; ok {
		p.dropLocked(userID, s)
	}
}

func (p *Pool) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.reap(time.Now())
		}
	}
}

// reap closes idle sessions and stops processes left without sessions.
func (p *Pool) reap(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, s := range p.sessions {
		if s.sink == nil && now.Sub(s.lastUsed) > p.cfg.IdleTimeout {
			p.dropLocked(uid, s)
		}
	}
	for _, pr := range p.procs {
		if pr.sessions <= 0 && now.Sub(pr.lastUsed) > p.cfg.IdleTimeout {
			pr.kill()
		}
	}
}

func (p *Pool) onNotify(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var n struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Status     string `json:"status"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &n) != nil {
		return
	}
	p.mu.Lock()
	s := p.byID[n.SessionID]
	var sink func(Update)
	if s != nil {
		sink = s.sink
	}
	p.mu.Unlock()
	if sink == nil {
		return // e.g. history replay during session/load
	}
	u := n.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if u.Content.Type == "text" {
			sink(Update{Kind: "token", Text: u.Content.Text})
		}
	case "tool_call":
		sink(Update{Kind: "tool_call", ToolCallID: u.ToolCallID, Title: u.Title, Status: u.Status})
	case "tool_call_update":
		sink(Update{Kind: "tool_call_update", ToolCallID: u.ToolCallID, Title: u.Title, Status: u.Status})
	}
}

// onRequest answers agent → client requests. Permission requests are granted
// because the MCP token already enforces what the agent may do; filesystem and
// terminal access is refused (not declared in capabilities).
func (p *Pool) onRequest(method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case "session/request_permission":
		var req struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(params, &req)
		for _, want := range []string{"allow_once", "allow_always"} {
			for _, o := range req.Options {
				if o.Kind == want {
					return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": o.OptionID}}, nil
				}
			}
		}
		return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
	default:
		return nil, &RPCError{Code: -32601, Message: "method not supported by Hammurapi: " + method}
	}
}

type logWriter struct{}

func (logWriter) Write(b []byte) (int, error) {
	slog.Debug("agent stderr", "line", string(b))
	return len(b), nil
}

// Caps returns the capabilities declared by the running agent processes
// (all processes run the same agent binary).
func (p *Pool) Caps() AgentCaps {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.procs {
		if !pr.dead {
			return pr.caps
		}
	}
	return AgentCaps{}
}
