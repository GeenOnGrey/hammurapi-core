package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Workspace runs one agent session with file system and terminal access
// confined to a working directory (PLT.HMR-0002 arch §6: only the runner allows
// fs/* and terminal/*; isolation of the environment is the executor's job).
type Workspace struct {
	Command string
	Args    []string
	Env     []string
	Root    string // absolute working directory of the task
	// MaxOutput bounds the retained output of a terminal.
	MaxOutput int

	mu    sync.Mutex
	terms map[string]*terminal
	seq   int
	sink  func(Update)
	usage Usage
}

// Usage is the token usage reported by the agent (or estimated).
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type terminal struct {
	cmd    *exec.Cmd
	out    *limitedBuffer
	done   chan struct{}
	code   *int
	signal *string
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if b.max > 0 && b.buf.Len() > b.max {
		keep := b.buf.Bytes()[b.buf.Len()-b.max:]
		nb := bytes.Buffer{}
		nb.Write(keep)
		b.buf = nb
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.truncated
}

// ErrOutsideWorkspace is returned for paths outside the task directory (RUN-09).
var ErrOutsideWorkspace = errors.New("path is outside the task working directory")

// Resolve maps a path from the agent to an absolute path inside Root.
func (w *Workspace) Resolve(p string) (string, error) {
	root, err := filepath.Abs(w.Root)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	clean := filepath.Clean(p)
	if !inside(root, clean) {
		return "", ErrOutsideWorkspace
	}
	// Symlinks must not lead outside either, including for a file that does not
	// exist yet: the nearest existing ancestor is resolved and the rest appended.
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if !inside(rr, realPath(clean)) {
		return "", ErrOutsideWorkspace
	}
	return clean, nil
}

func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath resolves symlinks in the longest existing prefix of p.
func realPath(p string) string {
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Run starts the agent, opens a session in Root with the MCP servers and sends
// the prompt. It returns the stop reason when the agent finishes the turn.
func (w *Workspace) Run(ctx context.Context, mcpServers []MCPServer, meta map[string]any, blocks []ContentBlock, onUpdate func(Update)) (string, error) {
	if w.Command == "" {
		return "", ErrNotConfigured
	}
	if w.MaxOutput <= 0 {
		w.MaxOutput = 256 << 10
	}
	w.terms = map[string]*terminal{}
	w.sink = onUpdate
	cmd := exec.CommandContext(ctx, w.Command, w.Args...)
	cmd.Env = append(os.Environ(), w.Env...)
	cmd.Dir = w.Root
	cmd.Stderr = &logWriter{}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start agent: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		w.killAll()
	}()
	c := newConn(stdout, stdin, w.onNotify, w.onRequest)
	go func() {
		err := cmd.Wait()
		c.close(fmt.Errorf("agent exited: %v", err))
	}()
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := c.call(ictx, "initialize", map[string]any{
		"protocolVersion":    ProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": true, "writeTextFile": true}, "terminal": true},
		"clientInfo":         map[string]string{"name": "hammurapi-runner", "version": "1.0.0"},
	}, nil); err != nil {
		return "", fmt.Errorf("agent initialize: %w", err)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "session/new", map[string]any{"cwd": w.Root, "mcpServers": mcpServers, "_meta": meta}, &sess); err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	var res struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"usage"`
		Meta map[string]any `json:"_meta"`
	}
	go func() {
		<-ctx.Done()
		_ = c.notify("session/cancel", map[string]string{"sessionId": sess.SessionID})
	}()
	if err := c.call(ctx, "session/prompt", map[string]any{"sessionId": sess.SessionID, "prompt": blocks}, &res); err != nil {
		return "", err
	}
	if res.Usage != nil {
		w.mu.Lock()
		w.usage = Usage{InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens}
		w.mu.Unlock()
	}
	return res.StopReason, nil
}

// Usage returns the reported usage (zero if the agent did not report it).
func (w *Workspace) Usage() Usage {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.usage
}

func (w *Workspace) onNotify(method string, params json.RawMessage) {
	if method != "session/update" || w.sink == nil {
		return
	}
	var n struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Status     string `json:"status"`
			Used       int64  `json:"used"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &n) != nil {
		return
	}
	u := n.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if u.Content.Type == "text" {
			w.sink(Update{Kind: "token", Text: u.Content.Text})
		}
	case "tool_call", "tool_call_update":
		w.sink(Update{Kind: u.SessionUpdate, ToolCallID: u.ToolCallID, Title: u.Title, Status: u.Status})
	case "usage_update":
		w.mu.Lock()
		if u.Used > w.usage.InputTokens {
			w.usage.InputTokens = u.Used
		}
		w.mu.Unlock()
	}
}

func rpcErr(err error) *RPCError {
	if errors.Is(err, ErrOutsideWorkspace) {
		return &RPCError{Code: -32602, Message: err.Error()}
	}
	return &RPCError{Code: -32603, Message: err.Error()}
}

func (w *Workspace) onRequest(method string, params json.RawMessage) (any, *RPCError) {
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
	case "fs/read_text_file":
		var req struct {
			Path  string `json:"path"`
			Line  *int   `json:"line"`
			Limit *int   `json:"limit"`
		}
		_ = json.Unmarshal(params, &req)
		p, err := w.Resolve(req.Path)
		if err != nil {
			return nil, rpcErr(err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, rpcErr(err)
		}
		content := string(b)
		if req.Line != nil || req.Limit != nil {
			lines := strings.Split(content, "\n")
			start := 0
			if req.Line != nil && *req.Line > 1 {
				start = min(*req.Line-1, len(lines))
			}
			end := len(lines)
			if req.Limit != nil && start+*req.Limit < end {
				end = start + *req.Limit
			}
			content = strings.Join(lines[start:end], "\n")
		}
		return map[string]string{"content": content}, nil
	case "fs/write_text_file":
		var req struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(params, &req)
		p, err := w.Resolve(req.Path)
		if err != nil {
			return nil, rpcErr(err)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, rpcErr(err)
		}
		if err := os.WriteFile(p, []byte(req.Content), 0o644); err != nil {
			return nil, rpcErr(err)
		}
		return map[string]any{}, nil
	case "terminal/create":
		var req struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Cwd     string   `json:"cwd"`
			Env     []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
			OutputByteLimit int `json:"outputByteLimit"`
		}
		_ = json.Unmarshal(params, &req)
		dir := w.Root
		if req.Cwd != "" {
			d, err := w.Resolve(req.Cwd)
			if err != nil {
				return nil, rpcErr(err)
			}
			dir = d
		}
		var cmd *exec.Cmd
		if len(req.Args) == 0 && strings.ContainsAny(req.Command, " |&;<>") {
			cmd = shellCommand(req.Command)
		} else {
			cmd = exec.Command(req.Command, req.Args...)
		}
		cmd.Dir = dir
		cmd.Env = os.Environ()
		for _, e := range req.Env {
			cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
		}
		limit := w.MaxOutput
		if req.OutputByteLimit > 0 && req.OutputByteLimit < limit {
			limit = req.OutputByteLimit
		}
		t := &terminal{cmd: cmd, out: &limitedBuffer{max: limit}, done: make(chan struct{})}
		cmd.Stdout, cmd.Stderr = t.out, t.out
		if err := cmd.Start(); err != nil {
			return nil, rpcErr(err)
		}
		w.mu.Lock()
		w.seq++
		id := fmt.Sprintf("term-%d", w.seq)
		w.terms[id] = t
		w.mu.Unlock()
		go func() {
			err := cmd.Wait()
			code := 0
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
			if err != nil && code == 0 {
				code = -1
			}
			t.code = &code
			close(t.done)
		}()
		return map[string]string{"terminalId": id}, nil
	case "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		var req struct {
			TerminalID string `json:"terminalId"`
		}
		_ = json.Unmarshal(params, &req)
		w.mu.Lock()
		t := w.terms[req.TerminalID]
		w.mu.Unlock()
		if t == nil {
			return nil, &RPCError{Code: -32602, Message: "unknown terminal"}
		}
		switch method {
		case "terminal/output":
			out, trunc := t.out.String()
			res := map[string]any{"output": out, "truncated": trunc}
			select {
			case <-t.done:
				res["exitStatus"] = map[string]any{"exitCode": t.code, "signal": t.signal}
			default:
			}
			return res, nil
		case "terminal/wait_for_exit":
			<-t.done
			return map[string]any{"exitCode": t.code, "signal": t.signal}, nil
		case "terminal/kill":
			if t.cmd.Process != nil {
				_ = t.cmd.Process.Kill()
			}
			return map[string]any{}, nil
		default: // release
			if t.cmd.Process != nil {
				_ = t.cmd.Process.Kill()
			}
			w.mu.Lock()
			delete(w.terms, req.TerminalID)
			w.mu.Unlock()
			return map[string]any{}, nil
		}
	}
	return nil, &RPCError{Code: -32601, Message: "method not supported by Hammurapi: " + method}
}

func (w *Workspace) killAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range w.terms {
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
	}
}
