package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GeenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
)

// ToolDeps are the dependencies of the MCP tools.
type ToolDeps struct {
	Store         specdata.Store
	Git           git.Provider
	Tokens        git.TokenSource
	Gates         *gates.Service
	Principal     PrincipalLoader
	DefaultBranch string
}

var both = []string{"general", "spec"}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var areaProp = map[string]any{"type": "string", "enum": []string{"product", "design", "arch", "tech", "qa"}}

// Tools builds Hammurapi's MCP tools. There is deliberately no delete tool:
// irreversible actions are performed only by the user.
func Tools(d ToolDeps) []mcp.Tool {
	return []mcp.Tool{
		{
			Name:        "list_features",
			Description: "List features of the product with their gate statuses. Optional filters: query (title or id substring), status (in_progress | handed_off | all), domain key.",
			Modes:       both,
			InputSchema: schema(map[string]any{
				"query":  map[string]any{"type": "string"},
				"status": map[string]any{"type": "string", "enum": []string{"in_progress", "handed_off", "all"}},
				"domain": map[string]any{"type": "string"},
			}),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Query, Status, Domain string }
				_ = json.Unmarshal(raw, &a)
				if a.Status == "" {
					a.Status = "all"
				}
				if a.Domain == "" {
					a.Domain = "all"
				}
				items, err := d.Store.ListFeatures(ctx, specdata.ListFilter{UserID: g.UserID, Domain: a.Domain, Status: a.Status, Query: a.Query, Page: httpx.Page{Limit: 100}})
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, f := range items {
					fmt.Fprintf(&b, "%s — %s [%s]", f.UniqueID, f.Title, f.Status)
					if f.ParentUniqueID != nil {
						fmt.Fprintf(&b, " fix of %s", *f.ParentUniqueID)
					}
					for _, gt := range f.Gates {
						fmt.Fprintf(&b, " %s:%s", gt.Area, gt.Status)
					}
					b.WriteString("\n")
				}
				if b.Len() == 0 {
					return "No features found.", nil
				}
				return b.String(), nil
			},
		},
		{
			Name:        "search_specs",
			Description: "Full-text search in specification documents merged to the default branch, plus feature titles. Use it to find where something is already described.",
			Modes:       both,
			InputSchema: schema(map[string]any{"query": map[string]any{"type": "string"}}, "query"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Query string }
				if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Query) == "" {
					return "", &mcp.ToolError{Msg: "query is required"}
				}
				var b strings.Builder
				items, err := d.Store.ListFeatures(ctx, specdata.ListFilter{UserID: g.UserID, Domain: "all", Status: "all", Query: a.Query, Page: httpx.Page{Limit: 20}})
				if err != nil {
					return "", err
				}
				for _, f := range items {
					fmt.Fprintf(&b, "feature %s — %s\n", f.UniqueID, f.Title)
				}
				token, err := d.Tokens.Token(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				hits, err := d.Git.SearchCode(ctx, token, a.Query)
				if err == nil {
					for _, h := range hits {
						fmt.Fprintf(&b, "%s: %s\n", h.Path, strings.ReplaceAll(truncate(h.Snippet, 300), "\n", " "))
					}
				} else {
					fmt.Fprintf(&b, "(document search unavailable: %s)\n", git.Reason(err))
				}
				if b.Len() == 0 {
					return "Nothing found.", nil
				}
				return b.String(), nil
			},
		},
		{
			Name:        "read_spec",
			Description: "Read the current markdown of a feature's gate document.",
			Modes:       both,
			InputSchema: schema(map[string]any{"uniqueId": map[string]any{"type": "string"}, "area": areaProp}, "uniqueId", "area"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct {
					UniqueID string `json:"uniqueId"`
					Area     string `json:"area"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", &mcp.ToolError{Msg: "invalid arguments"}
				}
				area, err := domain.ParseArea(a.Area)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				p, err := d.Principal(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				doc, err := d.Gates.GetDocument(ctx, p, a.UniqueID, area)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				return fmt.Sprintf("sha: %s\n\n%s", doc.SHA, doc.Content), nil
			},
		},
		{
			Name:        "read_rules",
			Description: "Read the rules template of an area: kind=template for regular features, kind=fix for fix features.",
			Modes:       both,
			InputSchema: schema(map[string]any{"area": areaProp, "kind": map[string]any{"type": "string", "enum": []string{"template", "fix"}}}, "area"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Area, Kind string }
				_ = json.Unmarshal(raw, &a)
				area, err := domain.ParseArea(a.Area)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				token, err := d.Tokens.Token(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				return features.Template(ctx, d.Git, token, d.DefaultBranch, area, a.Kind == "fix"), nil
			},
		},
		{
			Name: "edit_spec",
			Description: "Replace the full markdown of a gate document of the current feature. Allowed only in specification mode, " +
				"in areas where the user is an editor, and only for gates that are not approved. Supported markdown: CommonMark + GFM " +
				"(tables, task lists, strikethrough), no raw HTML or footnotes; keep front matter of fix documents.",
			Modes:       []string{"spec"},
			InputSchema: schema(map[string]any{"area": areaProp, "content": map[string]any{"type": "string"}}, "area", "content"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Area, Content string }
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", &mcp.ToolError{Msg: "invalid arguments"}
				}
				area, err := domain.ParseArea(a.Area)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				if err := CheckEdit(ctx, d.Store, g, area); err != nil {
					return "", err
				}
				p, err := d.Principal(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				res, err := d.Gates.SaveDocument(ctx, p, g.Feature, area, gates.SaveInput{Content: a.Content}, true)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				if res.Commit == nil {
					return "No changes: the document already has this content.", nil
				}
				return fmt.Sprintf("Saved %s/%s as commit %s. The gate returns to draft.", g.Feature, area, *res.Commit), nil
			},
		},
	}
}

// CheckEdit enforces the edit_spec rules on the grant: spec mode, editor role in
// the area, and a gate that exists and is not approved.
func CheckEdit(ctx context.Context, store specdata.Store, g mcp.Grant, area domain.Area) error {
	if g.Mode != "spec" {
		return &mcp.ToolError{Msg: "editing is only possible in specification mode; ask the user to switch the chat mode"}
	}
	if !g.CanEditArea(area) {
		return &mcp.ToolError{Msg: fmt.Sprintf("the user is not an editor of the %s area", area)}
	}
	f, err := store.FeatureByUniqueID(ctx, g.Feature)
	if errors.Is(err, specdata.ErrNotFound) {
		return &mcp.ToolError{Msg: "feature not found"}
	}
	if err != nil {
		return err
	}
	if f.Status != domain.FeatureInProgress {
		return &mcp.ToolError{Msg: "the feature is handed off or deleted; changes go through a fix feature"}
	}
	gt, err := store.ActiveGate(ctx, f.ID, area)
	if errors.Is(err, specdata.ErrNotFound) {
		return &mcp.ToolError{Msg: fmt.Sprintf("the feature has no %s gate; the user can add it in the interface", area)}
	}
	if err != nil {
		return err
	}
	if gt.Status == domain.GateApproved {
		return &mcp.ToolError{Msg: "the gate is approved; the agent does not edit approved gates"}
	}
	return nil
}
