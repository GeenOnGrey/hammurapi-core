package acp

import (
	"os"
	"path/filepath"
	"testing"
)

// RUN-09: the agent cannot touch files outside the task directory.
func TestWorkspaceResolve(t *testing.T) {
	root := t.TempDir()
	w := &Workspace{Root: root}
	if p, err := w.Resolve("src/main.go"); err != nil || p != filepath.Join(root, "src", "main.go") {
		t.Fatalf("got %s %v", p, err)
	}
	for _, bad := range []string{"../etc/passwd", filepath.Join(root, "..", "x"), filepath.Dir(root)} {
		if _, err := w.Resolve(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err == nil {
		// Neither path exists yet: the check must not depend on the target existing.
		for _, bad := range []string{"link/secret", "link/new/deep/file"} {
			if _, err := w.Resolve(bad); err == nil {
				t.Errorf("symlink escape accepted: %s", bad)
			}
		}
	}
	if _, rerr := w.onRequest("fs/write_text_file", []byte(`{"path":"../evil.txt","content":"x"}`)); rerr == nil {
		t.Error("write outside accepted")
	}
	if _, rerr := w.onRequest("fs/write_text_file", []byte(`{"path":"a/b.txt","content":"ok"}`)); rerr != nil {
		t.Fatalf("write inside refused: %v", rerr)
	}
	res, rerr := w.onRequest("fs/read_text_file", []byte(`{"path":"a/b.txt"}`))
	if rerr != nil || res.(map[string]string)["content"] != "ok" {
		t.Fatalf("read: %v %v", res, rerr)
	}
}
