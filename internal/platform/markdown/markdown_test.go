package markdown

import (
	"strings"
	"testing"
)

func TestRenderTemplateReplacesTitlePlaceholder(t *testing.T) {
	got := RenderTemplate("# Product spec: <feature title>\n\n## Problem\n", "Weekend booking", "")
	if !strings.HasPrefix(got, "# Product spec: Weekend booking\n") {
		t.Fatalf("unexpected heading: %q", got)
	}
}

func TestRenderTemplateFixSetsParent(t *testing.T) {
	tpl := "---\nparent: <parent id>\n---\n\n# Fix: <what changes>\n"
	got := RenderTemplate(tpl, "Refund amount", "PAY.INV-0009")
	front, body, ok := SplitFrontMatter(got)
	if !ok || front["parent"] != "PAY.INV-0009" {
		t.Fatalf("front matter not set: %q", got)
	}
	if !strings.HasPrefix(body, "# Fix: Refund amount") {
		t.Fatalf("title not rendered: %q", body)
	}
}

func TestRenderTemplateWithoutHeadingAddsTitle(t *testing.T) {
	got := RenderTemplate("## Problem\n", "X", "")
	if !strings.HasPrefix(got, "# X\n\n## Problem") {
		t.Fatalf("got %q", got)
	}
}

func TestCheckSubset(t *testing.T) {
	cases := []struct {
		doc  string
		fix  bool
		want []string
	}{
		{"# A\n\ntext", false, nil},
		{"# A\n\n<div>x</div>", false, []string{"raw_html"}},
		{"# A\n\n```html\n<div>x</div>\n```", false, nil},
		{"# A\n\nuse `<br>` here", false, nil},
		{"# A\n\nnote[^1]\n\n[^1]: x", false, []string{"footnotes"}},
		{"---\nparent: X\n---\n# A", false, []string{"front_matter_not_allowed"}},
		{"---\nparent: X\n---\n# A", true, nil},
	}
	for _, c := range cases {
		got := CheckSubset(c.doc, c.fix)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("CheckSubset(%q) = %v, want %v", c.doc, got, c.want)
		}
	}
}

func TestDiffGroupsBySection(t *testing.T) {
	a := "# T\n\n## Welcome\n\nFive steps.\n\n## States\n\nWaiting."
	b := "# T\n\n## Welcome\n\nThree steps.\nExplain first.\n\n## States\n\nWaiting."
	d := Diff(a, b)
	var plus, minus int
	for _, l := range d {
		switch l.Op {
		case "+":
			plus++
			if l.Section != "Welcome" {
				t.Errorf("added line %q in section %q", l.Text, l.Section)
			}
		case "-":
			minus++
		}
	}
	if plus != 2 || minus != 1 {
		t.Fatalf("plus=%d minus=%d, diff=%+v", plus, minus, d)
	}
}
