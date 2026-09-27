package git

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestBlobSHA(t *testing.T) {
	// `printf 'hello\n' | git hash-object --stdin`
	if got := BlobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("got %s", got)
	}
}

func TestTrailersRoundTrip(t *testing.T) {
	msg := Trailers{Feature: "FMS.CAR-0005", Area: "product", Agent: true, Import: "abc", Delete: "design"}.Message("subject")
	got := ParseTrailers(msg)
	if got.Feature != "FMS.CAR-0005" || got.Area != "product" || !got.Agent || got.Import != "abc" || got.Delete != "design" {
		t.Fatalf("got %+v from %q", got, msg)
	}
}

func TestParseSpecPath(t *testing.T) {
	loc, ok := ParseSpecPath("specs/FMS/CAR/FMS.CAR-0005/product/spec.md")
	if !ok || loc.UniqueID != "FMS.CAR-0005" || loc.Area != "product" || loc.Rest != "spec.md" {
		t.Fatalf("got %+v %v", loc, ok)
	}
	if _, ok := ParseSpecPath("specs/FMS/CAR/spec.md"); ok {
		t.Fatal("short path accepted")
	}
}

// HOOK-01: webhook secrets.
func TestVerifyWebhook(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/feature/A.B-0001"}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	gh := NewGitHub("https://github.com", "https://github.com", "o/r", "", "")
	if !gh.VerifyWebhook(h, body, "s3cret") {
		t.Fatal("valid signature rejected")
	}
	if gh.VerifyWebhook(h, body, "other") || gh.VerifyWebhook(http.Header{}, body, "s3cret") {
		t.Fatal("invalid signature accepted")
	}
	gl := NewGitLab("https://gitlab.com", "https://gitlab.com", "o/r", "", "")
	h = http.Header{}
	h.Set("X-Gitlab-Token", "s3cret")
	if !gl.VerifyWebhook(h, body, "s3cret") || gl.VerifyWebhook(h, body, "x") || gl.VerifyWebhook(http.Header{}, body, "") {
		t.Fatal("gitlab token check")
	}
}

func TestParsePushGitLab(t *testing.T) {
	gl := NewGitLab("https://gitlab.com", "https://gitlab.com", "o/r", "", "")
	h := http.Header{}
	h.Set("X-Gitlab-Event", "Push Hook")
	h.Set("X-Gitlab-Event-UUID", "u1")
	ev, ok, err := gl.ParsePush(h, []byte(`{"ref":"refs/heads/feature/FMS.CAR-0005","user_username":"anna",
		"commits":[{"id":"c1","message":"m","added":["a"],"modified":["b"],"removed":[]}]}`))
	if err != nil || !ok || ev.Branch != "feature/FMS.CAR-0005" || ev.Actor != "anna" || ev.EventID != "gitlab:u1" || len(ev.Commits) != 1 {
		t.Fatalf("got %+v %v %v", ev, ok, err)
	}
	h.Set("X-Gitlab-Event", "Merge Request Hook")
	if _, ok, _ := gl.ParsePush(h, []byte(`{}`)); ok {
		t.Fatal("non-push parsed")
	}
}

// fakeGitHub records the git data API calls of a commit.
type fakeGitHub struct {
	mu    sync.Mutex
	calls []string
	tree  []map[string]any
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v3/repos/o/r/git/ref/heads/feature/X":
		w.Write([]byte(`{"object":{"sha":"head"}}`))
	case r.Method == "GET" && r.URL.Path == "/api/v3/repos/o/r/git/commits/head":
		w.Write([]byte(`{"tree":{"sha":"tree0"}}`))
	case r.Method == "POST" && r.URL.Path == "/api/v3/repos/o/r/git/blobs":
		w.Write([]byte(`{"sha":"blob1"}`))
	case r.Method == "POST" && r.URL.Path == "/api/v3/repos/o/r/git/trees":
		var req struct {
			BaseTree string           `json:"base_tree"`
			Tree     []map[string]any `json:"tree"`
		}
		_ = json.Unmarshal(body, &req)
		f.tree = req.Tree
		w.Write([]byte(`{"sha":"tree1"}`))
	case r.Method == "POST" && r.URL.Path == "/api/v3/repos/o/r/git/commits":
		w.Write([]byte(`{"sha":"commit1"}`))
	case r.Method == "PATCH" && r.URL.Path == "/api/v3/repos/o/r/git/refs/heads/feature/X":
		w.Write([]byte(`{}`))
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/repos/o/r/contents/"):
		w.Write([]byte(`{"type":"file","sha":"s","encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("# doc\n")) + `"}`))
	case r.Method == "PUT" && r.URL.Path == "/api/v3/repos/o/r/pulls/7/merge":
		w.WriteHeader(405)
		w.Write([]byte(`{"message":"Required status check is expected"}`))
	default:
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func TestGitHubCommitFlow(t *testing.T) {
	f := &fakeGitHub{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	gh := NewGitHub(srv.URL, srv.URL, "o/r", "", "")
	sha, err := gh.Commit(context.Background(), "tok", "feature/X", "msg",
		[]FileChange{{Path: "specs/a/spec.md", Content: []byte("x")}, {Path: "specs/b/spec.md", Delete: true}})
	if err != nil || sha != "commit1" {
		t.Fatalf("got %s %v (calls %v)", sha, err, f.calls)
	}
	if len(f.tree) != 2 || f.tree[0]["sha"] != "blob1" || f.tree[1]["sha"] != nil {
		t.Fatalf("tree %v", f.tree)
	}
	file, err := gh.GetFile(context.Background(), "tok", "feature/X", "specs/a/spec.md")
	if err != nil || string(file.Content) != "# doc\n" {
		t.Fatalf("get file: %v %v", file, err)
	}
	// Provider refusals keep the reason for the user.
	err = gh.MergePR(context.Background(), "tok", 7, "m")
	if Reason(err) != "Required status check is expected" {
		t.Fatalf("got %v", err)
	}
	if _, err := gh.BranchHead(context.Background(), "bad", "feature/X"); err != ErrUnauthorized {
		t.Fatalf("got %v", err)
	}
}

// GitLab picks create vs update per file.
func TestGitLabCommitActions(t *testing.T) {
	var actions []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.RawPath+r.URL.Path, "existing"):
			w.WriteHeader(200)
		case r.Method == http.MethodHead:
			w.WriteHeader(404)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/repository/commits"):
			var req struct {
				Actions []map[string]string `json:"actions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			actions = req.Actions
			w.Write([]byte(`{"id":"c9"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	gl := NewGitLab(srv.URL, srv.URL, "o/r", "", "")
	sha, err := gl.Commit(context.Background(), "tok", "feature/X", "m", []FileChange{
		{Path: "specs/existing.md", Content: []byte("a")}, {Path: "specs/new.md", Content: []byte("b")}, {Path: "specs/gone.md", Delete: true}})
	if err != nil || sha != "c9" {
		t.Fatalf("got %s %v", sha, err)
	}
	if actions[0]["action"] != "update" || actions[1]["action"] != "create" || actions[2]["action"] != "delete" {
		t.Fatalf("actions %v", actions)
	}
}
