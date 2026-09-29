package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Scenarios of Hammurapi's agent contexts, chosen by the prompt header
// "[hammurapi:task=<name> key=value …]" (PLT.HMR-0002): discovery,
// generate_gates, implement, address_review, update_pr, check.

var (
	headerRe   = regexp.MustCompile(`^\[hammurapi:task=([a-z_]+)([^\]]*)\]`)
	kvRe       = regexp.MustCompile(`(\w+)=(\S+)`)
	reqRe      = regexp.MustCompile(`\*\*R(\d+)\.\*\*`)
	reqLineRe  = regexp.MustCompile(`(?m)^- (R\d+): `)
	tcLineRe   = regexp.MustCompile(`(?m)^- ([A-Z]+-\d+) \[`)
	serviceRe  = regexp.MustCompile(`(?m)^- ([a-z0-9._-]+) \(system ([A-Z0-9]+/[A-Z0-9]+|-), repo`)
	featureRe  = regexp.MustCompile(`feature (FTR\.([A-Z0-9]+)\.([A-Z0-9]+)-\d+)`)
	issueTitle = regexp.MustCompile(`issue (ISS\.[A-Z0-9]+-\d+) "([^"]*)"`)
)

func header(text string) (string, map[string]string, bool) {
	m := headerRe.FindStringSubmatch(text)
	if m == nil {
		return "", nil, false
	}
	kv := map[string]string{}
	for _, x := range kvRe.FindAllStringSubmatch(m[2], -1) {
		kv[x[1]] = x[2]
	}
	return m[1], kv, true
}

func tool(session, name string, args any) string {
	res, err := callMCP(session, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "error: " + err.Error()
	}
	return res
}

func writeFile(session, path, content string) error {
	r := request("fs/write_text_file", map[string]any{"sessionId": session, "path": path, "content": content})
	if len(r.Error) > 0 {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}

func scenario(session, task string, kv map[string]string, text string) {
	switch task {
	case "discovery":
		title := "the issue"
		if m := issueTitle.FindStringSubmatch(text); m != nil {
			title = m[2]
		}
		source := os.Getenv("FAKEAGENT_METRIC_SOURCE")
		if source == "" {
			source = "demo"
		}
		content := fmt.Sprintf("## Value\n\n%s makes the product more useful for the users of the domain.\n\n"+
			"## How we measure\n\n| Source | Query | Target | Window |\n| --- | --- | --- | --- |\n| %s | `sum(hammurapi_demo_value)` | +5%% | 14d |\n\n"+
			"## Similar issues and features\n\nNone found.\n\n## Affected systems and services\n\nSee the catalog.\n\n## Risks and size\n\nSmall.\n", title, source)
		if kv["type"] == "problem" {
			content += "\n## Cause hypothesis\n\nA recent change broke the flow.\n"
		}
		chunk(session, "Researching… ")
		res := tool(session, "save_discovery", map[string]any{"content": content, "value": title + " increases the value metric",
			"measure": map[string]string{"source": source, "query": "sum(hammurapi_demo_value)", "target": "+5%", "window": "14d"}})
		chunk(session, "Discovery saved: "+short(res))
	case "generate_gates":
		reqs := uniqueSorted(reqRe.FindAllStringSubmatch(text, -1), "R")
		if len(reqs) == 0 {
			reqs = []string{"R1"}
		}
		system := ""
		if m := featureRe.FindStringSubmatch(text); m != nil {
			system = m[2] + "/" + m[3]
		}
		var services []string
		for _, m := range serviceRe.FindAllStringSubmatch(text, -1) {
			if m[2] == system {
				services = append(services, m[1])
			}
		}
		if len(services) == 0 {
			for _, m := range serviceRe.FindAllStringSubmatch(text, -1) {
				services = append(services, m[1])
				if len(services) == 2 {
					break
				}
			}
		}
		for _, area := range strings.Split(kv["areas"], ",") {
			var b strings.Builder
			switch area {
			case "tech":
				b.WriteString("# Technical specification\n\n## Changes by service\n\n| Service | Changes | Requirements |\n| --- | --- | --- |\n")
				for _, s := range services {
					fmt.Fprintf(&b, "| %s | Implement the feature in %s | %s |\n", s, s, strings.Join(reqs, ", "))
				}
				b.WriteString("\n## Rollout order\n\n")
				for i, s := range services {
					fmt.Fprintf(&b, "%d. %s\n", i+1, s)
				}
			case "qa":
				b.WriteString("# Test specification\n\n## Test cases\n\n| ID | Requirements | Scenario | Expected result | Level |\n| --- | --- | --- | --- | --- |\n")
				for i, r := range reqs {
					fmt.Fprintf(&b, "| QA-%02d | %s | Check %s | Works as specified | U |\n", i+1, r, r)
				}
			default:
				continue
			}
			res := tool(session, "submit_gate", map[string]string{"area": area, "content": b.String()})
			chunk(session, area+": "+short(res)+"\n")
		}
	case "implement", "address_review", "update_pr":
		feature := kv["feature"]
		slug := strings.ToLower(strings.NewReplacer(".", "_", "-", "_").Replace(feature))
		tool(session, "report_progress", map[string]string{"message": "reading the specification"})
		if kv["autonomy"] == "plan" && task == "implement" {
			plan := fmt.Sprintf("# Plan for %s\n\n1. Add the handler.\n2. Add tests for the test cases.\n", feature)
			if err := writeFile(session, "HAMMURAPI_PLAN.md", plan); err != nil {
				chunk(session, "write failed: "+err.Error())
				return
			}
			chunk(session, "Plan written.")
			return
		}
		var reqs, tcs []string
		for _, m := range reqLineRe.FindAllStringSubmatch(text, -1) {
			reqs = append(reqs, m[1])
		}
		for _, m := range tcLineRe.FindAllStringSubmatch(text, -1) {
			tcs = append(tcs, m[1])
		}
		code := fmt.Sprintf("package main\n\n// %s implements %s (%s).\nfunc %s() string { return %q }\n", "feature"+slug, feature, strings.Join(reqs, ", "), "feature"+slug, feature)
		if task != "implement" {
			code += fmt.Sprintf("\n// %s: revision after %s.\n", slug, task)
		}
		var tests strings.Builder
		tests.WriteString("package main\n\nimport \"testing\"\n")
		for _, tc := range tcs {
			fmt.Fprintf(&tests, "\nfunc Test%s_%s(t *testing.T) {\n\tif feature%s() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n", strings.ReplaceAll(tc, "-", ""), slug, slug)
		}
		if err := writeFile(session, "feature_"+slug+".go", code); err != nil {
			chunk(session, "write failed: "+err.Error())
			return
		}
		if err := writeFile(session, "feature_"+slug+"_test.go", tests.String()); err != nil {
			chunk(session, "write failed: "+err.Error())
			return
		}
		tool(session, "report_progress", map[string]string{"message": "tests written"})
		chunk(session, "Done. Implemented: "+strings.Join(reqs, ", "))
	case "check":
		chunk(session, "The code matches the specification.")
	default:
		chunk(session, "unknown task "+task)
	}
}

func uniqueSorted(ms [][]string, prefix string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		id := prefix + m[1]
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) < len(out[j]) || (len(out[i]) == len(out[j]) && out[i] < out[j]) })
	return out
}

func short(s string) string {
	var r struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(s), &r) == nil && len(r.Result.Content) > 0 {
		if r.Result.IsError {
			return "error: " + r.Result.Content[0].Text
		}
		return r.Result.Content[0].Text
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
