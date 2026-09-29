// Package agentrun runs one-off agent sessions in the worker (PLT.HMR-0002
// arch §6): Discovery, generation of tech and qa, and the code check. Every
// session gets its own MCP token whose grant limits the tools and the objects;
// structured results come back through the grant's sink.
package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/platform/acp"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/mcp"
)

// Runner starts agent sessions.
type Runner struct {
	Agent acp.AgentClient
	MCP   *mcp.Server
	URL   string // MCP endpoint reachable by the agent process
}

// Outcome is the result of a session.
type Outcome struct {
	Text      string
	TokensIn  int64
	TokensOut int64
	// Results are the sink calls by kind, in order.
	Results map[string][]json.RawMessage
}

// Last returns the last result of a kind.
func (o Outcome) Last(kind string) (json.RawMessage, bool) {
	rs := o.Results[kind]
	if len(rs) == 0 {
		return nil, false
	}
	return rs[len(rs)-1], true
}

// ErrNoAgent means the agent is not configured.
var ErrNoAgent = acp.ErrNotConfigured

// MCPServerFor returns the MCP server entry for a session: HTTP when the agent
// supports it, otherwise the stdio proxy of the hammurapi binary.
func MCPServerFor(caps acp.AgentCaps, url, token string) acp.MCPServer {
	if caps.HTTPMCP {
		return acp.MCPServer{Type: "http", Name: "hammurapi", URL: url,
			Headers: []acp.NameValue{{Name: "Authorization", Value: "Bearer " + token}}}
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "hammurapi"
	}
	return acp.MCPServer{Name: "hammurapi", Command: exe, Args: []string{"mcp-proxy"},
		Env: []acp.NameValue{{Name: "HAMMURAPI_MCP_URL", Value: url}, {Name: "HAMMURAPI_MCP_TOKEN", Value: token}}}
}

// Once runs a single prompt in a fresh session and closes it.
func (r *Runner) Once(ctx context.Context, g mcp.Grant, meta map[string]any, prompt string) (Outcome, error) {
	if r.Agent == nil {
		return Outcome{}, ErrNoAgent
	}
	out := Outcome{Results: map[string][]json.RawMessage{}}
	var mu sync.Mutex
	g.Sink = func(kind string, payload json.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		out.Results[kind] = append(out.Results[kind], append(json.RawMessage(nil), payload...))
		return nil
	}
	token := r.MCP.Issue(g)
	defer r.MCP.Revoke(token)
	key := uuid.New() // a session of its own, not a user's chat session
	defer r.Agent.CloseSession(key)
	if _, err := r.Agent.Ensure(ctx, key, func(caps acp.AgentCaps) acp.SessionSetup {
		return acp.SessionSetup{MCPServers: []acp.MCPServer{MCPServerFor(caps, r.URL, token)}, Meta: map[string]any{"hammurapi": meta}}
	}); err != nil {
		return out, err
	}
	var b strings.Builder
	_, err := r.Agent.Prompt(ctx, key, []acp.ContentBlock{acp.TextBlock(prompt)}, func(u acp.Update) {
		switch u.Kind {
		case "token":
			mu.Lock()
			b.WriteString(u.Text)
			mu.Unlock()
		case "error":
			mu.Lock()
			b.WriteString("\n[agent error] " + u.Text)
			mu.Unlock()
		}
	})
	mu.Lock()
	out.Text = b.String()
	mu.Unlock()
	// ACP has no standard usage report yet: estimate by characters (≈4 per token).
	out.TokensIn, out.TokensOut = int64(len(prompt)/4+1), int64(len(out.Text)/4+1)
	if err != nil {
		return out, err
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, ctx.Err()
	}
	return out, nil
}
