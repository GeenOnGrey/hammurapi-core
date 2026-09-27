// Command fakeagent is a scripted Agent Client Protocol agent for tests and
// local smoke runs. It speaks JSON-RPC over stdio and answers deterministically:
//
//   - any text          → echoes "echo: <text>" in two chunks
//   - "crash"           → exits with status 1 mid-answer
//   - "fs"              → asks the client for fs/read_text_file and reports the result
//   - "tools"           → calls MCP tools/list on the session's HTTP MCP server
//   - "edit <area>: <markdown>" → calls the edit_spec MCP tool
//
// It is not a real agent; do not use it in production.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type mcpServer struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
}

var (
	out      = bufio.NewWriter(os.Stdout)
	outMu    sync.Mutex
	nextID   atomic.Int64
	pending  = map[int64]chan msg{}
	pendMu   sync.Mutex
	sessions = map[string][]mcpServer{}
	sessMu   sync.Mutex
)

func send(v any) {
	b, _ := json.Marshal(v)
	outMu.Lock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
	outMu.Unlock()
}

func reply(id json.RawMessage, result any) {
	send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func request(method string, params any) msg {
	id := nextID.Add(1) + 1000
	ch := make(chan msg, 1)
	pendMu.Lock()
	pending[id] = ch
	pendMu.Unlock()
	send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return <-ch
}

func chunk(session, text string) {
	send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": session,
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}},
	}})
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var m msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" && m.ID != nil { // response to our request
			var id int64
			_ = json.Unmarshal(m.ID, &id)
			pendMu.Lock()
			ch := pending[id]
			delete(pending, id)
			pendMu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		go handle(m)
	}
}

func handle(m msg) {
	switch m.Method {
	case "initialize":
		reply(m.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession":        false,
				"promptCapabilities": map[string]bool{"image": false, "embeddedContext": true},
				"mcpCapabilities":    map[string]bool{"http": true},
			},
			"authMethods": []any{},
		})
	case "session/new":
		var p struct {
			MCPServers []mcpServer `json:"mcpServers"`
		}
		_ = json.Unmarshal(m.Params, &p)
		id := fmt.Sprintf("fake-%d", nextID.Add(1))
		sessMu.Lock()
		sessions[id] = p.MCPServers
		sessMu.Unlock()
		reply(m.ID, map[string]string{"sessionId": id})
	case "session/prompt":
		var p struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.Unmarshal(m.Params, &p)
		text := ""
		if n := len(p.Prompt); n > 0 {
			text = strings.TrimSpace(p.Prompt[n-1].Text)
		}
		answer(p.SessionID, text)
		reply(m.ID, map[string]string{"stopReason": "end_turn"})
	case "session/cancel":
	default:
		if m.ID != nil {
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}

func answer(session, text string) {
	switch {
	case text == "crash":
		chunk(session, "going down…")
		os.Exit(1)
	case text == "fs":
		r := request("fs/read_text_file", map[string]any{"sessionId": session, "path": "/etc/passwd"})
		if len(r.Error) > 0 {
			chunk(session, "fs refused: "+string(r.Error))
		} else {
			chunk(session, "fs allowed: "+string(r.Result))
		}
	case text == "tools":
		res, err := callMCP(session, "tools/list", map[string]any{})
		if err != nil {
			chunk(session, "mcp error: "+err.Error())
			return
		}
		chunk(session, "tools: "+res)
	case strings.HasPrefix(text, "edit "):
		area, content, _ := strings.Cut(strings.TrimPrefix(text, "edit "), ":")
		res, err := callMCP(session, "tools/call", map[string]any{"name": "edit_spec",
			"arguments": map[string]string{"area": strings.TrimSpace(area), "content": strings.TrimSpace(content) + "\n"}})
		if err != nil {
			chunk(session, "mcp error: "+err.Error())
			return
		}
		chunk(session, "edit result: "+res)
	default:
		half := len(text) / 2
		chunk(session, "echo: "+text[:half])
		chunk(session, text[half:])
	}
}

func callMCP(session, method string, params any) (string, error) {
	sessMu.Lock()
	servers := sessions[session]
	sessMu.Unlock()
	for _, s := range servers {
		if s.Type != "http" {
			continue
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, s.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for _, h := range s.Headers {
			req.Header.Set(h.Name, h.Value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b), nil
	}
	return "", fmt.Errorf("no http MCP server in session")
}
