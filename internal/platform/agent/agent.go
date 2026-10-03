// Package agent is the contract of the agent operator (HMR.CMN-0004 arch §3):
// the internal API `agent:8090/v1` that api, worker and runner use to run Pi
// sessions, its stream events, the scenarios and the classes of LLM errors.
// The operator itself lives in agent/operator, the Pi adapter in agent/pi.
package agent

import (
	"encoding/json"
	"fmt"
)

// Scenario is what the agent is doing (tech spec §1).
type Scenario string

// Scenarios of HMR.CMN-0004 §4.
const (
	ScenarioChat             Scenario = "chat"
	ScenarioIssueAnalysis    Scenario = "issue_analysis"
	ScenarioGateGeneration   Scenario = "gate_generation"
	ScenarioConformanceCheck Scenario = "conformance_check"
	ScenarioCodegen          Scenario = "codegen"
	ScenarioReviewUpdate     Scenario = "review_update"
	ScenarioRollbackRevert   Scenario = "rollback_revert"
)

// Scenarios lists every scenario in the order of the H-SDLC stages.
var Scenarios = []Scenario{ScenarioChat, ScenarioIssueAnalysis, ScenarioGateGeneration, ScenarioConformanceCheck,
	ScenarioCodegen, ScenarioReviewUpdate, ScenarioRollbackRevert}

// Valid reports whether s is a known scenario.
func (s Scenario) Valid() bool {
	for _, x := range Scenarios {
		if x == s {
			return true
		}
	}
	return false
}

// HasCode reports whether the scenario works on a repository checkout in a
// runner task: the built-in file and shell tools run there.
func (s Scenario) HasCode() bool {
	return s == ScenarioCodegen || s == ScenarioReviewUpdate || s == ScenarioRollbackRevert
}

// ParseScenario validates a scenario name.
func ParseScenario(s string) (Scenario, error) {
	if !Scenario(s).Valid() {
		return "", fmt.Errorf("unknown scenario %q", s)
	}
	return Scenario(s), nil
}

// ErrorClass is the class of an LLM failure (arch §8).
type ErrorClass string

// Error classes.
const (
	ErrInsufficientBalance ErrorClass = "insufficient_balance"
	ErrAuth                ErrorClass = "auth"
	ErrRateLimit           ErrorClass = "rate_limit"
	ErrUnavailable         ErrorClass = "unavailable"
	ErrBadRequest          ErrorClass = "bad_request"
	ErrContextOverflow     ErrorClass = "context_overflow"
	ErrAgentCrashed        ErrorClass = "agent_crashed"
)

// Retryable reports whether the user may retry later without an
// administrator fixing the connection.
func (c ErrorClass) Retryable() bool {
	return c == ErrRateLimit || c == ErrUnavailable || c == ErrAgentCrashed || c == ErrContextOverflow
}

// ConnectionProblem reports whether the class marks the connection as broken
// (shown to global administrators in "In focus").
func (c ErrorClass) ConnectionProblem() bool { return c == ErrInsufficientBalance || c == ErrAuth }

// SessionKind separates the operator's capacity: chat and worker sessions vs
// long runner tasks (arch §4.5).
type SessionKind string

// Session kinds.
const (
	KindChat SessionKind = "chat"
	KindTask SessionKind = "task"
)

// Cost is a price in US dollars per million tokens.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// ModelDef is one model of a connection as Pi needs it in models.json.
type ModelDef struct {
	ID               string             `json:"id"`
	Name             string             `json:"name,omitempty"`
	ContextWindow    int64              `json:"contextWindow"`
	MaxTokens        int64              `json:"maxTokens"`
	Reasoning        bool               `json:"reasoning"`
	Input            []string           `json:"input"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap,omitempty"`
	Cost             Cost               `json:"cost"`
	Compat           map[string]any     `json:"compat,omitempty"`
}

// ThinkingLevels lists the levels Hammurapi offers for the model: "off" and
// the levels mapped to a provider value.
func (m ModelDef) ThinkingLevels() []string {
	levels := []string{"off"}
	if !m.Reasoning {
		return levels
	}
	for _, l := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		if v, ok := m.ThinkingLevelMap[l]; ok && v != nil {
			levels = append(levels, l)
		}
	}
	return levels
}

// SupportsThinking reports whether level is offered for the model.
func (m ModelDef) SupportsThinking(level string) bool {
	for _, l := range m.ThinkingLevels() {
		if l == level {
			return true
		}
	}
	return false
}

// ModelSpec is the resolved model of a session: the provider (one per
// connection) with its models, and the model and level to use.
type ModelSpec struct {
	Provider     string     `json:"provider"` // hmr-<connection id>
	ConnectionID string     `json:"connectionId"`
	API          string     `json:"api"`
	BaseURL      string     `json:"baseUrl"`
	ModelID      string     `json:"modelId"`
	Thinking     string     `json:"thinking"`
	Models       []ModelDef `json:"models"`
}

// Secrets travel only in the request body and live in the Pi process
// environment; files on disk reference them as ${…}.
type Secrets struct {
	LLMKey     string                       `json:"llmKey"`
	MCPToken   string                       `json:"mcpToken,omitempty"`
	MCPHeaders map[string]map[string]string `json:"mcpHeaders,omitempty"` // server → header → value
}

// MCPServer is an external MCP server of the session (streamable HTTP).
type MCPServer struct {
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	HeaderNames []string `json:"headerNames,omitempty"`
	Exposure    string   `json:"exposure"` // direct | deferred
}

// Skills names the skills snapshot and the skills of the scenario.
type Skills struct {
	Hash   string   `json:"hash"`
	Names  []string `json:"names"`
	Bundle []byte   `json:"bundle,omitempty"` // tar.gz, sent when the operator does not know the hash
}

// Workspace is the runner's workspace server of a code scenario.
type Workspace struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// HistoryMessage seeds a chat session that has no snapshot.
type HistoryMessage struct {
	Role string `json:"role"` // user | assistant
	Text string `json:"text"`
}

// SessionRequest is the body of POST /v1/sessions.
type SessionRequest struct {
	Scenario        Scenario         `json:"scenario"`
	Kind            SessionKind      `json:"kind"`
	Model           ModelSpec        `json:"model"`
	Secrets         Secrets          `json:"secrets"`
	MCP             []MCPServer      `json:"mcp,omitempty"`
	HammurapiMCPURL string           `json:"hammurapiMcpUrl"`
	Skills          *Skills          `json:"skills,omitempty"`
	SystemAppend    string           `json:"systemAppend,omitempty"`
	Snapshot        []byte           `json:"snapshot,omitempty"`
	History         []HistoryMessage `json:"history,omitempty"`
	Workspace       *Workspace       `json:"workspace,omitempty"`
	// Label is a short description for the operator's logs (never secrets).
	Label string `json:"label,omitempty"`
}

// SessionResponse is the answer of POST /v1/sessions.
type SessionResponse struct {
	SessionID    string `json:"sessionId"`
	SessionToken string `json:"sessionToken"`
	Model        string `json:"model"`
	Thinking     string `json:"thinking"`
}

// Image is an image attached to a prompt.
type Image struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

// PromptRequest is the body of POST /v1/sessions/{id}/prompt.
type PromptRequest struct {
	Text   string  `json:"text"`
	Images []Image `json:"images,omitempty"`
}

// PatchRequest changes the model or level of a session.
type PatchRequest struct {
	ModelID  string `json:"modelId,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// Event types of the operator's NDJSON stream (tech spec §4.3). They are the
// operator's own contract, independent of Pi's event names.
const (
	EventTextDelta     = "text_delta"
	EventThinkingDelta = "thinking_delta"
	EventToolCall      = "tool_call"
	EventToolResult    = "tool_result"
	EventCompaction    = "compaction"
	EventRetry         = "retry"
	EventUsage         = "usage"
	EventError         = "error"
	EventSettled       = "settled"
)

// Event is one record of the prompt stream.
type Event struct {
	Type         string          `json:"type"`
	Delta        string          `json:"delta,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	IsError      bool            `json:"isError,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	TokensBefore int64           `json:"tokensBefore,omitempty"`
	TokensAfter  int64           `json:"tokensAfter,omitempty"`
	Attempt      int             `json:"attempt,omitempty"`
	ErrorClass   ErrorClass      `json:"errorClass,omitempty"`
	HTTPStatus   int             `json:"httpStatus,omitempty"`
	Message      string          `json:"message,omitempty"`
	// Aborted marks a settled run stopped by abort (the user's cancel, the
	// runner's token limit): the stream still ends normally.
	Aborted bool `json:"aborted,omitempty"`
	Usage
}

// Usage is token use and cost.
type Usage struct {
	TokensIn   int64   `json:"tokensIn,omitempty"`
	TokensOut  int64   `json:"tokensOut,omitempty"`
	CacheRead  int64   `json:"cacheRead,omitempty"`
	CacheWrite int64   `json:"cacheWrite,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"`
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.TokensIn += u2.TokensIn
	u.TokensOut += u2.TokensOut
	u.CacheRead += u2.CacheRead
	u.CacheWrite += u2.CacheWrite
	u.CostUSD += u2.CostUSD
}

// IsZero reports whether nothing was used.
func (u Usage) IsZero() bool {
	return u.TokensIn == 0 && u.TokensOut == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && u.CostUSD == 0
}

// LLMCheckRequest is the body of POST /v1/checks/llm.
type LLMCheckRequest struct {
	Model   ModelSpec `json:"model"`
	Secrets Secrets   `json:"secrets"`
}

// LLMCheckResult is the result for one model.
type LLMCheckResult struct {
	Model      string     `json:"model"`
	OK         bool       `json:"ok"`
	LatencyMs  int64      `json:"latencyMs,omitempty"`
	ErrorClass ErrorClass `json:"errorClass,omitempty"`
	HTTPStatus int        `json:"httpStatus,omitempty"`
	Message    string     `json:"message,omitempty"`
}

// LLMCheckResponse is the answer of POST /v1/checks/llm.
type LLMCheckResponse struct {
	Results []LLMCheckResult `json:"results"`
}

// MCPCheckRequest is the body of POST /v1/checks/mcp.
type MCPCheckRequest struct {
	Server  MCPServer         `json:"server"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPTool is a tool reported by an MCP server.
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"readOnly"`
	Destructive bool   `json:"destructive"`
}

// MCPCheckResponse is the answer of POST /v1/checks/mcp.
type MCPCheckResponse struct {
	OK    bool      `json:"ok"`
	Tools []MCPTool `json:"tools"`
	Error string    `json:"error,omitempty"`
}
