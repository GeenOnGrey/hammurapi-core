// Package markdown contains server-side helpers for spec documents: front
// matter, template rendering, the supported-subset check and line diffs.
package markdown

import (
	"fmt"
	"regexp"
	"strings"
)

// SplitFrontMatter separates a leading "---" YAML block from the body.
func SplitFrontMatter(doc string) (front map[string]string, body string, ok bool) {
	norm := strings.ReplaceAll(doc, "\r\n", "\n")
	if !strings.HasPrefix(norm, "---\n") {
		return nil, doc, false
	}
	end := strings.Index(norm[4:], "\n---")
	if end < 0 {
		return nil, doc, false
	}
	block := norm[4 : 4+end]
	rest := norm[4+end+4:]
	rest = strings.TrimLeft(rest, "\n")
	front = map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		k, v, found := strings.Cut(line, ":")
		if found {
			front[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return front, rest, true
}

var placeholderRe = regexp.MustCompile(`<[^<>\n]+>`)

// RenderTemplate turns a rules template into the first version of a gate document:
// the first heading's <placeholder> becomes the feature title, and for fix
// documents the front matter gets parent: <parent id>.
func RenderTemplate(tpl, title, parentID string) string {
	tpl = strings.ReplaceAll(tpl, "\r\n", "\n")
	front, body, hasFront := SplitFrontMatter(tpl)
	lines := strings.Split(body, "\n")
	replaced := false
	for i, l := range lines {
		if strings.HasPrefix(l, "# ") {
			if placeholderRe.MatchString(l) {
				lines[i] = placeholderRe.ReplaceAllLiteralString(l, title)
			} else {
				lines[i] = strings.TrimRight(l, " ") + ": " + title
			}
			replaced = true
			break
		}
	}
	body = strings.Join(lines, "\n")
	if !replaced {
		body = "# " + title + "\n\n" + body
	}
	if parentID == "" {
		return strings.TrimLeft(body, "\n")
	}
	if !hasFront {
		front = map[string]string{}
	}
	front["parent"] = parentID
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "parent: %s\n", front["parent"])
	for k, v := range front {
		if k != "parent" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimLeft(body, "\n"))
	return b.String()
}

// FirstHeading returns the text of the first "# " heading.
func FirstHeading(doc string) string {
	_, body, _ := SplitFrontMatter(doc)
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "# "))
		}
	}
	return ""
}

var (
	htmlTagRe  = regexp.MustCompile(`(?m)</?[a-zA-Z][a-zA-Z0-9-]*(\s[^<>]*)?/?>`)
	footnoteRe = regexp.MustCompile(`\[\^[^\]]+\]`)
	codeSpanRe = regexp.MustCompile("`[^`\n]*`")
)

// CheckSubset reports constructs outside CommonMark+GFM supported by the editor:
// raw HTML and footnotes. Front matter is allowed only in fix documents.
func CheckSubset(doc string, fix bool) []string {
	var warnings []string
	_, body, hasFront := SplitFrontMatter(doc)
	if hasFront && !fix {
		warnings = append(warnings, "front_matter_not_allowed")
	}
	stripped := stripCode(body)
	if htmlTagRe.MatchString(stripped) {
		warnings = append(warnings, "raw_html")
	}
	if footnoteRe.MatchString(stripped) {
		warnings = append(warnings, "footnotes")
	}
	return warnings
}

// stripCode removes fenced blocks and code spans, where HTML is just text.
func stripCode(s string) string {
	var out []string
	inFence := false
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence {
			out = append(out, codeSpanRe.ReplaceAllString(l, ""))
		}
	}
	return strings.Join(out, "\n")
}

// DiffLine is one line of a unified line diff.
type DiffLine struct {
	Op      string `json:"op"` // "=", "+", "-"
	Text    string `json:"text"`
	Section string `json:"section,omitempty"` // nearest heading above the line
}

// Diff computes a line diff with LCS. Each line carries the nearest heading
// so the UI can group changes by section.
func Diff(a, b string) []DiffLine {
	al := splitLines(a)
	bl := splitLines(b)
	n, m := len(al), len(bl)
	// LCS table; documents are small (hundreds of lines).
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []DiffLine
	section := ""
	track := func(s string) {
		if strings.HasPrefix(s, "#") {
			section = strings.TrimSpace(strings.TrimLeft(s, "#"))
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && al[i] == bl[j]:
			track(bl[j])
			out = append(out, DiffLine{Op: "=", Text: al[i], Section: section})
			i++
			j++
		case j < m && (i == n || dp[i][j+1] >= dp[i+1][j]):
			track(bl[j])
			out = append(out, DiffLine{Op: "+", Text: bl[j], Section: section})
			j++
		default:
			out = append(out, DiffLine{Op: "-", Text: al[i], Section: section})
			i++
		}
	}
	return out
}

func splitLines(s string) []string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
