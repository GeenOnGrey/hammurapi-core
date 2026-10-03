package agentcfg

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
)

const goodMD = "---\nname: review-checklist\ndescription: Checklist for reviewing specifications\n---\n# Review\n"

func zipOf(t *testing.T, files map[string]string, symlink string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if symlink != "" {
		h := &zip.FileHeader{Name: symlink}
		h.SetMode(0o777 | fs.ModeSymlink)
		w, _ := zw.CreateHeader(h)
		_, _ = w.Write([]byte("/etc/passwd"))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func reasons(t *testing.T, err error) string {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Code != "skill_invalid" {
		t.Fatalf("err = %v", err)
	}
	return strings.Join(e.Details["details"].([]string), "; ")
}

func TestSkillZipValid(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"top directory": {"review-checklist/SKILL.md": goodMD, "review-checklist/scripts/check.sh": "echo ok"},
		"root":          {"SKILL.md": goodMD, "notes.md": "x"},
	} {
		sk, err := ParseSkillZip(zipOf(t, files, ""))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sk.Name != "review-checklist" || len(sk.Files) != 2 {
			t.Fatalf("%s: %+v", name, sk)
		}
		for _, f := range sk.Files {
			if strings.HasPrefix(f.Path, "review-checklist/") {
				t.Fatalf("%s: path not relative to the skill: %s", name, f.Path)
			}
		}
	}
}

// SK-06: no SKILL.md, a name with an underscore, an empty description.
func TestSkillInvalid(t *testing.T) {
	if r := reasons(t, func() error { _, err := ParseSkillZip(zipOf(t, map[string]string{"x/readme.md": "x"}, "")); return err }()); !strings.Contains(r, "SKILL.md is missing") {
		t.Fatal(r)
	}
	_, err := ParseSkillText("---\nname: review_checklist\ndescription: \"\"\n---\n")
	r := reasons(t, err)
	if !strings.Contains(r, "name must be") || !strings.Contains(r, "description is required") {
		t.Fatal(r)
	}
	_, err = ParseSkillText("no frontmatter")
	if r := reasons(t, err); !strings.Contains(r, "frontmatter") {
		t.Fatal(r)
	}
	_, err = ParseSkillText("---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\n")
	reasons(t, err)
}

// SK-07: a path with "..", a link, an archive over 5 MB, unpacked over 20 MB.
func TestSkillArchiveLimits(t *testing.T) {
	if r := reasons(t, func() error {
		_, err := ParseSkillZip(zipOf(t, map[string]string{"s/SKILL.md": goodMD, "s/../../evil": "x"}, ""))
		return err
	}()); !strings.Contains(r, "unsafe path") {
		t.Fatal(r)
	}
	if r := reasons(t, func() error {
		_, err := ParseSkillZip(zipOf(t, map[string]string{"s/SKILL.md": goodMD}, "s/link"))
		return err
	}()); !strings.Contains(r, "links") {
		t.Fatal(r)
	}
	if r := reasons(t, func() error { _, err := ParseSkillZip(make([]byte, maxSkillZip+1)); return err }()); !strings.Contains(r, "5 MB") {
		t.Fatal(r)
	}
	big := strings.Repeat("a", 21<<20) // compresses well: small zip, large unpacked
	if r := reasons(t, func() error {
		_, err := ParseSkillZip(zipOf(t, map[string]string{"s/SKILL.md": goodMD, "s/big.txt": big}, ""))
		return err
	}()); !strings.Contains(r, "20 MB") {
		t.Fatal(r)
	}
	if r := reasons(t, func() error {
		_, err := ParseSkillZip(zipOf(t, map[string]string{"a/SKILL.md": goodMD, "b/x.md": "x"}, ""))
		return err
	}()); !strings.Contains(r, "one skill directory") {
		t.Fatal(r)
	}
}

// MCP-08: http outside the cluster, SSE and stdio are rejected; MCP-06: the built-in name.
func TestMCPValidation(t *testing.T) {
	ok := []string{"https://mcp.example.com/jira", "http://jira-mcp.tools.svc:8080/mcp", "http://x.ns.svc.cluster.local/mcp"}
	for _, u := range ok {
		in := MCPInput{Name: "jira", URL: u}
		if err := in.validate(true); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	bad := []string{"http://mcp.example.com/jira", "https://mcp.example.com/sse", "npx -y @mcp/server", "stdio://x", ""}
	for _, u := range bad {
		in := MCPInput{Name: "jira", URL: u}
		if e, ok := apperr.As(in.validate(true)); !ok || e.Code != "mcp_invalid" {
			t.Errorf("%q accepted", u)
		}
	}
	in := MCPInput{Name: "hammurapi", URL: "https://x"}
	if e, ok := apperr.As(in.validate(true)); !ok || e.Code != "mcp_builtin" {
		t.Fatal("built-in name accepted")
	}
	in = MCPInput{Name: "jira", URL: "https://x", Headers: make([]HeaderInput, 11)}
	if e, ok := apperr.As(in.validate(true)); !ok || e.Code != "mcp_invalid" {
		t.Fatal("11 headers accepted")
	}
	in = MCPInput{Name: "jira", URL: "https://x", Exposure: "codemode"}
	if in.validate(true) == nil {
		t.Fatal("unknown exposure accepted")
	}
}

func TestPresetModels(t *testing.T) {
	pr := Presets[TypeDeepSeek]
	all, ok := presetModels(pr, nil)
	if !ok || len(all) != 2 || all[0].ID != "deepseek-v4-flash" || all[1].ID != "deepseek-v4-pro" {
		t.Fatalf("%+v", all)
	}
	if _, ok := presetModels(pr, []string{"gpt-5"}); ok {
		t.Fatal("unknown model accepted")
	}
	// MOD-02: DeepSeek V4 offers off, high, xhigh only.
	if got := strings.Join(all[0].ThinkingLevels(), ","); got != "off,high,xhigh" {
		t.Fatal(got)
	}
	if all[1].Cost.Input != 1.74 || all[1].Cost.Output != 3.48 || all[1].Cost.CacheRead != 0.145 {
		t.Fatalf("pro prices %+v", all[1].Cost)
	}
}

func TestSkillsConfigRender(t *testing.T) {
	cfg := skillsConfig{Skills: map[string]struct {
		Scenarios []agent.Scenario `yaml:"scenarios"`
	}{"b": {Scenarios: []agent.Scenario{agent.ScenarioChat}}, "a": {Scenarios: []agent.Scenario{agent.ScenarioCodegen, agent.ScenarioChat}}}}
	out := string(renderSkillsConfig(cfg))
	if !strings.Contains(out, "  a:\n    scenarios: [codegen, chat]\n  b:\n    scenarios: [chat]\n") {
		t.Fatal(out)
	}
	if !strings.HasSuffix(string(renderSkillsConfig(skillsConfig{})), "skills: {}\n") {
		t.Fatal("empty config")
	}
}
