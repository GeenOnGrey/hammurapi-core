package git

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// IDX-16: a truncated recursive tree is walked level by level from dir, with
// the same result.
func TestGitHubTreeTruncated(t *testing.T) {
	levels := map[string][]map[string]any{
		"main":    {{"path": "specs", "type": "tree", "sha": "t-specs"}, {"path": "README.md", "type": "blob", "sha": "b0", "size": 3}},
		"t-specs": {{"path": "FMS", "type": "tree", "sha": "t-fms"}},
		"t-fms":   {{"path": "a.md", "type": "blob", "sha": "b1", "size": 10}, {"path": "CAR", "type": "tree", "sha": "t-car"}},
		"t-car":   {{"path": "spec.md", "type": "blob", "sha": "b2", "size": 20}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sha := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/o/r/git/trees/")
		if strings.HasPrefix(r.URL.Path, "/api/v3/repos/o/r/git/blobs/") {
			_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte("# Title\n")) + "\n", "encoding": "base64"})
			return
		}
		if r.URL.Query().Get("recursive") == "1" {
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": []any{}, "truncated": true})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": levels[sha]})
	}))
	defer srv.Close()
	gh := NewGitHub(srv.URL, srv.URL, "o/r", "", "")
	got, err := gh.Tree(context.Background(), "tok", "main", "specs")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range got {
		paths = append(paths, e.Path+"@"+e.SHA)
	}
	sort.Strings(paths)
	if strings.Join(paths, " ") != "specs/FMS/CAR/spec.md@b2 specs/FMS/a.md@b1" {
		t.Fatalf("%v", paths)
	}
	b, err := gh.Blob(context.Background(), "tok", "b2")
	if err != nil || string(b) != "# Title\n" {
		t.Fatalf("blob %q %v", b, err)
	}
}

func TestGitLabTreeAndBlob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repository/tree"):
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "b1", "path": "specs/A/spec.md", "type": "blob"}, {"id": "t", "path": "specs/A", "type": "tree"}})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "b2", "path": "specs/B/spec.md", "type": "blob"}})
		case strings.HasSuffix(r.URL.Path, "/repository/blobs/b2/raw"):
			_, _ = w.Write([]byte("raw content"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	gl := NewGitLab(srv.URL, srv.URL, "o/r", "", "")
	got, err := gl.Tree(context.Background(), "tok", "main", "specs")
	if err != nil || len(got) != 2 || got[1].SHA != "b2" || got[0].Size != -1 {
		t.Fatalf("%+v %v", got, err)
	}
	b, err := gl.Blob(context.Background(), "tok", "b2")
	if err != nil || string(b) != "raw content" {
		t.Fatalf("blob %q %v", b, err)
	}
}
