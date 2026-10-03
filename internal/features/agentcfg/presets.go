// Package agentcfg is the Agent section of the administration (PLT.HMR-0004
// R5–R21): LLM connections, the model of every scenario, MCP servers, skills
// (through PRs to the specification repository), usage and the audit log. It
// also resolves, for a scenario, everything a Pi session needs: the model, the
// decrypted keys, the MCP servers and the skills.
package agentcfg

import "github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"

// ConnectionType is a kind of LLM connection; the first release has DeepSeek.
type ConnectionType string

// Connection types.
const TypeDeepSeek ConnectionType = "deepseek"

// Preset is the fixed part of a connection type.
type Preset struct {
	Type    ConnectionType
	Name    string
	BaseURL string
	API     string
	Models  []agent.ModelDef
}

func strp(s string) *string { return &s }

// deepseekCompat follows DeepSeek's official configuration for Pi (tech spec §2.1).
var deepseekCompat = map[string]any{
	"supportsStore": false, "supportsDeveloperRole": false, "maxTokensField": "max_tokens",
	"requiresReasoningContentOnAssistantMessages": true, "thinkingFormat": "deepseek",
}

// deepseekLevels: high → high, xhigh → max; the other levels are not supported.
var deepseekLevels = map[string]*string{"minimal": nil, "low": nil, "medium": nil, "high": strp("high"), "xhigh": strp("max")}

// Presets by type. Prices are US dollars per million tokens.
var Presets = map[ConnectionType]Preset{
	TypeDeepSeek: {
		Type: TypeDeepSeek, Name: "DeepSeek", BaseURL: "https://api.deepseek.com", API: "openai-completions",
		Models: []agent.ModelDef{
			{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextWindow: 1_000_000, MaxTokens: 384_000, Reasoning: true,
				Input: []string{"text"}, ThinkingLevelMap: deepseekLevels, Compat: deepseekCompat,
				Cost: agent.Cost{Input: 0.14, Output: 0.28, CacheRead: 0.028}},
			{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextWindow: 1_000_000, MaxTokens: 384_000, Reasoning: true,
				Input: []string{"text"}, ThinkingLevelMap: deepseekLevels, Compat: deepseekCompat,
				Cost: agent.Cost{Input: 1.74, Output: 3.48, CacheRead: 0.145}},
		},
	},
}

// presetModels returns the preset models restricted to ids (all when ids is empty).
func presetModels(p Preset, ids []string) ([]agent.ModelDef, bool) {
	if len(ids) == 0 {
		return append([]agent.ModelDef(nil), p.Models...), true
	}
	byID := map[string]agent.ModelDef{}
	for _, m := range p.Models {
		byID[m.ID] = m
	}
	out := make([]agent.ModelDef, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// StageOf groups scenarios by H-SDLC stage for the interface (design §3).
var StageOf = map[agent.Scenario]string{
	agent.ScenarioChat:             "all",
	agent.ScenarioIssueAnalysis:    "discovery",
	agent.ScenarioGateGeneration:   "development",
	agent.ScenarioConformanceCheck: "development",
	agent.ScenarioCodegen:          "development",
	agent.ScenarioReviewUpdate:     "development",
	agent.ScenarioRollbackRevert:   "delivery",
}
