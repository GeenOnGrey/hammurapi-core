// Package mcp is a minimal Model Context Protocol server (Streamable HTTP with
// JSON responses) through which the agent reaches Hammurapi tools. Every ACP
// session gets its own bearer token; the token's Grant decides what the agent
// may do on behalf of the user.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
)

// ProtocolVersion is the MCP revision implemented.
const ProtocolVersion = "2025-06-18"

// Grant is the permission scope of an MCP token.
type Grant struct {
	UserID uuid.UUID
	// Mode is "general" or "spec".
	Mode string
	// Feature is the uniqueId of the feature in spec mode.
	Feature string
	// Area is the currently open area, if any.
	Area domain.Area
	// EditorAreas are areas where the user is an editor.
	EditorAreas []domain.Area
}

// CanEditArea reports whether the grant allows edit_spec in area a.
func (g Grant) CanEditArea(a domain.Area) bool {
	if g.Mode != "spec" || g.Feature == "" {
		return false
	}
	for _, x := range g.EditorAreas {
		if x == a {
			return true
		}
	}
	return false
}

// Tool is an MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Modes lists the chat modes in which the tool is offered.
	Modes   []string
	Handler func(ctx context.Context, g Grant, args json.RawMessage) (string, error)
}

func (t Tool) availableIn(mode string) bool {
	for _, m := range t.Modes {
		if m == mode {
			return true
		}
	}
	return false
}

// ToolError is a tool failure reported to the agent as isError content.
type ToolError struct{ Msg string }

func (e *ToolError) Error() string { return e.Msg }

// Server holds tools and tokens.
type Server struct {
	mu     sync.RWMutex
	grants map[string]Grant
	tools  []Tool
}

// NewServer creates a server.
func NewServer() *Server { return &Server{grants: map[string]Grant{}} }

// Register adds tools.
func (s *Server) Register(tools ...Tool) { s.tools = append(s.tools, tools...) }

// Issue creates a token for a grant.
func (s *Server) Issue(g Grant) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.grants[tok] = g
	s.mu.Unlock()
	return tok
}

// Update replaces the grant of a token (mode or feature switch).
func (s *Server) Update(tok string, g Grant) {
	s.mu.Lock()
	if _, ok := s.grants[tok]; ok {
		s.grants[tok] = g
	}
	s.mu.Unlock()
}

// Revoke deletes a token.
func (s *Server) Revoke(tok string) {
	s.mu.Lock()
	delete(s.grants, tok)
	s.mu.Unlock()
}

func (s *Server) grant(tok string) (Grant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.grants[tok]
	return g, ok
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ServeHTTP handles POST /mcp.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, ok := s.grant(tok)
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcErr{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted) // notification
		return
	}
	res, rerr := s.dispatch(r.Context(), g, req)
	writeRPC(w, req.ID, res, rerr)
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcErr) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		resp["error"] = e
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, g Grant, req request) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "hammurapi", "version": "1.0.0"},
			"instructions":    "Hammurapi specification tools. Use search_specs and read_spec to research; edit_spec to change a gate document (spec mode only). Deleting specifications is not possible through tools: the user deletes them in the Hammurapi UI.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := []map[string]any{}
		for _, t := range s.tools {
			if t.availableIn(g.Mode) {
				list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
			}
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{Code: -32602, Message: "invalid params"}
		}
		for _, t := range s.tools {
			if t.Name != p.Name {
				continue
			}
			if !t.availableIn(g.Mode) {
				return toolResult(fmt.Sprintf("Tool %s is not available in %s mode. Ask the user to switch the chat to specification mode.", t.Name, g.Mode), true), nil
			}
			out, err := t.Handler(ctx, g, p.Arguments)
			if err != nil {
				slog.InfoContext(ctx, "mcp tool refused", "tool", t.Name, "err", err)
				return toolResult(err.Error(), true), nil
			}
			return toolResult(out, false), nil
		}
		return nil, &rpcErr{Code: -32602, Message: "unknown tool " + p.Name}
	default:
		return nil, &rpcErr{Code: -32601, Message: "method not found"}
	}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}

// RunStdioProxy bridges MCP over stdio to the HTTP endpoint for agents that only
// support stdio MCP servers. Invoked as `hammurapi mcp-proxy`.
func RunStdioProxy(ctx context.Context, url, token string) error {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	out := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(line))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && len(bytes.TrimSpace(b)) > 0 {
			out.Write(bytes.TrimSpace(b))
			out.WriteByte('\n')
			out.Flush()
		}
	}
	return sc.Err()
}
