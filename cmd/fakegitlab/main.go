// Command fakegitlab is an in-memory imitation of the parts of the GitLab API
// Hammurapi uses: OAuth sign-in, branches, files, trees, multi-file commits,
// merge requests, issues, code search, and push webhooks back to Hammurapi.
//
// It exists for end-to-end tests and local demos without a real GitLab. It is
// not a GitLab replacement and must never be exposed publicly.
//
// Configuration (env):
//
//	FAKE_ADDR        listen address (default :8929)
//	FAKE_PUBLIC_URL  URL browsers use to reach it (default http://localhost:8929)
//	FAKE_WEBHOOK_URL Hammurapi webhook endpoint, e.g. http://api:8080/hooks/v1/git
//	WEBHOOK_SECRET   sent as X-Gitlab-Token
//	FAKE_SEED_DIR    directory whose files seed the default branch (e.g. rules/)
//	FAKE_USERS       "login:Display Name,…" offered on the sign-in page
//	FAKE_REFUSE_MERGE=1  refuse merges with 405 (to test provider refusals)
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // git ids
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type commit struct {
	ID      string
	Parent  string
	Message string
	Author  string
	Time    time.Time
	Files   map[string][]byte
}

type mr struct {
	IID       int
	Source    string
	Target    string
	Title     string
	State     string // opened | merged | closed
	Approvals []string
}

type server struct {
	mu       sync.Mutex
	commits  map[string]*commit
	branches map[string]string // branch -> commit id
	mrs      map[int]*mr
	nextMR   int
	issues   int
	users    map[string]string // login -> name
	tokens   map[string]string // access token -> login
	public   string
	webhook  string
	secret   string
	refuse   bool
}

func main() {
	s := &server{
		commits: map[string]*commit{}, branches: map[string]string{}, mrs: map[int]*mr{}, nextMR: 1,
		users: map[string]string{}, tokens: map[string]string{},
		public:  strings.TrimRight(env("FAKE_PUBLIC_URL", "http://localhost:8929"), "/"),
		webhook: os.Getenv("FAKE_WEBHOOK_URL"), secret: os.Getenv("WEBHOOK_SECRET"),
		refuse: os.Getenv("FAKE_REFUSE_MERGE") == "1",
	}
	for _, u := range strings.Split(env("FAKE_USERS", "admin:Admin,anna:Anna K.,oleg:Oleg D."), ",") {
		login, name, _ := strings.Cut(strings.TrimSpace(u), ":")
		if login != "" {
			s.users[login] = name
		}
	}
	root := &commit{ID: newID(), Message: "Initial commit", Author: "fakegitlab", Time: time.Now(), Files: map[string][]byte{}}
	if dir := os.Getenv("FAKE_SEED_DIR"); dir != "" {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(filepath.Dir(dir), p)
			data, err := os.ReadFile(p)
			if err == nil {
				root.Files[filepath.ToSlash(rel)] = data
			}
			return nil
		})
	}
	s.commits[root.ID] = root
	s.branches["main"] = root.ID
	addr := env("FAKE_ADDR", ":8929")
	log.Printf("fakegitlab listening on %s (%d seed files)", addr, len(root.Files))
	log.Fatal(http.ListenAndServe(addr, s))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func newID() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func blobID(data []byte) string {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// segments splits the escaped path so encoded slashes (project ids, file paths) survive.
func segments(r *http.Request) []string {
	parts := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	for i, p := range parts {
		if u, err := url.PathUnescape(p); err == nil {
			parts[i] = u
		}
	}
	return parts
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := segments(r)
	switch {
	case r.URL.Path == "/oauth/authorize":
		s.authorize(w, r)
	case r.URL.Path == "/oauth/token":
		s.token(w, r)
	case len(p) >= 2 && p[0] == "api" && p[1] == "v4":
		s.mu.Lock()
		defer s.mu.Unlock()
		login, ok := s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			fail(w, 401, "401 Unauthorized")
			return
		}
		s.api(w, r, p[2:], login)
	case r.URL.Path == "/healthz":
		_, _ = w.Write([]byte("ok"))
	default:
		fail(w, 404, "404 Not Found")
	}
}

// ─── OAuth ──────────────────────────────────────────────────────────

func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if user := q.Get("user"); user != "" {
		s.mu.Lock()
		if _, ok := s.users[user]; !ok {
			s.users[user] = user
		}
		s.mu.Unlock()
		u, _ := url.Parse(q.Get("redirect_uri"))
		v := u.Query()
		v.Set("code", "code-"+user)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}
	s.mu.Lock()
	logins := make([]string, 0, len(s.users))
	for l := range s.users {
		logins = append(logins, l)
	}
	s.mu.Unlock()
	sort.Strings(logins)
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><title>Fake GitLab sign-in</title>
<body style="font-family:system-ui;max-width:420px;margin:60px auto"><h2>Fake GitLab (development only)</h2><p>Sign in as:</p>`)
	for _, l := range logins {
		v := r.URL.Query()
		v.Set("user", l)
		fmt.Fprintf(&b, `<p><a href="/oauth/authorize?%s">%s (@%s)</a></p>`, html.EscapeString(v.Encode()), html.EscapeString(s.users[l]), html.EscapeString(l))
	}
	b.WriteString(`<form method="get" action="/oauth/authorize">`)
	for k, vs := range r.URL.Query() {
		fmt.Fprintf(&b, `<input type="hidden" name="%s" value="%s">`, html.EscapeString(k), html.EscapeString(vs[0]))
	}
	b.WriteString(`<input name="user" placeholder="another login"> <button>Sign in</button></form></body>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (s *server) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	var login string
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		login = strings.TrimPrefix(r.Form.Get("code"), "code-")
	case "refresh_token":
		login = strings.TrimPrefix(r.Form.Get("refresh_token"), "refresh-")
	}
	if login == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	tok := "tok-" + login + "-" + newID()[:8]
	s.mu.Lock()
	s.tokens[tok] = login
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"access_token": tok, "refresh_token": "refresh-" + login, "token_type": "bearer", "expires_in": 7200})
}

// ─── API ────────────────────────────────────────────────────────────

func (s *server) api(w http.ResponseWriter, r *http.Request, p []string, login string) {
	if len(p) == 1 && p[0] == "user" {
		writeJSON(w, 200, map[string]any{"id": userID(login), "username": login, "name": s.users[login], "avatar_url": ""})
		return
	}
	if len(p) < 3 || p[0] != "projects" {
		fail(w, 404, "404 Not Found")
		return
	}
	rest := p[2:] // after projects/:id
	q := r.URL.Query()
	switch {
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "branches" && r.Method == http.MethodGet:
		id, ok := s.branches[rest[2]]
		if !ok {
			fail(w, 404, "404 Branch Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"name": rest[2], "commit": map[string]string{"id": id}})
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "branches" && r.Method == http.MethodPost:
		name, ref := q.Get("branch"), q.Get("ref")
		if _, exists := s.branches[name]; exists {
			fail(w, 400, "Branch already exists")
			return
		}
		if _, ok := s.commits[ref]; !ok {
			if id, ok := s.branches[ref]; ok {
				ref = id
			} else {
				fail(w, 400, "Invalid reference name")
				return
			}
		}
		s.branches[name] = ref
		writeJSON(w, 201, map[string]any{"name": name, "commit": map[string]string{"id": ref}})
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "branches" && r.Method == http.MethodDelete:
		if _, ok := s.branches[rest[2]]; !ok {
			fail(w, 404, "404 Branch Not Found")
			return
		}
		delete(s.branches, rest[2])
		w.WriteHeader(204)
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "files":
		c := s.resolve(q.Get("ref"))
		if c == nil {
			fail(w, 404, "404 Commit Not Found")
			return
		}
		data, ok := c.Files[rest[2]]
		if !ok {
			fail(w, 404, "404 File Not Found")
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(200)
			return
		}
		writeJSON(w, 200, map[string]any{"file_path": rest[2], "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(data), "blob_id": blobID(data), "last_commit_id": c.ID})
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "tree":
		c := s.resolve(q.Get("ref"))
		if c == nil {
			fail(w, 404, "404 Tree Not Found")
			return
		}
		prefix := strings.Trim(q.Get("path"), "/")
		var out []map[string]string
		for path := range c.Files {
			if prefix == "" || strings.HasPrefix(path, prefix+"/") {
				out = append(out, map[string]string{"path": path, "type": "blob", "name": filepath.Base(path)})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["path"] < out[j]["path"] })
		writeJSON(w, 200, out)
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "commits" && r.Method == http.MethodPost:
		s.commit(w, r, login)
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "commits" && r.Method == http.MethodGet:
		c := s.resolve(q.Get("ref_name"))
		path := strings.Trim(q.Get("path"), "/")
		var out []map[string]string
		for c != nil && len(out) < 1 {
			parent := s.commits[c.Parent]
			if path == "" || touches(c, parent, path) {
				out = append(out, map[string]string{"id": c.ID, "message": c.Message})
			}
			c = parent
		}
		if out == nil {
			out = []map[string]string{}
		}
		writeJSON(w, 200, out)
	case len(rest) == 1 && rest[0] == "merge_requests" && r.Method == http.MethodPost:
		var in struct {
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			Title        string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		m := &mr{IID: s.nextMR, Source: in.SourceBranch, Target: in.TargetBranch, Title: in.Title, State: "opened"}
		s.mrs[m.IID] = m
		s.nextMR++
		writeJSON(w, 201, map[string]any{"iid": m.IID, "web_url": fmt.Sprintf("%s/demo/-/merge_requests/%d", s.public, m.IID)})
	case len(rest) >= 2 && rest[0] == "merge_requests":
		iid, _ := strconv.Atoi(rest[1])
		m, ok := s.mrs[iid]
		if !ok {
			fail(w, 404, "404 Not Found")
			return
		}
		switch {
		case len(rest) == 3 && rest[2] == "merge":
			if s.refuse {
				fail(w, 405, "Method Not Allowed: branch is protected")
				return
			}
			if m.State != "opened" {
				fail(w, 405, "Merge request is not open")
				return
			}
			src := s.resolve(m.Source)
			if src == nil {
				fail(w, 406, "Source branch does not exist")
				return
			}
			files := map[string][]byte{}
			for k, v := range src.Files {
				files[k] = v
			}
			c := &commit{ID: newID(), Parent: s.branches[m.Target], Message: "Merge branch '" + m.Source + "'", Author: login, Time: time.Now(), Files: files}
			s.commits[c.ID] = c
			s.branches[m.Target] = c.ID
			m.State = "merged"
			delete(s.branches, m.Source) // "delete source branch" like the default MR option
			writeJSON(w, 200, map[string]any{"iid": iid, "state": "merged"})
		case len(rest) == 3 && rest[2] == "approve":
			m.Approvals = append(m.Approvals, login)
			writeJSON(w, 201, map[string]any{"iid": iid})
		case len(rest) == 2 && r.Method == http.MethodPut:
			var in struct {
				StateEvent string `json:"state_event"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.StateEvent == "close" {
				m.State = "closed"
			}
			writeJSON(w, 200, map[string]any{"iid": iid, "state": m.State})
		default:
			fail(w, 404, "404 Not Found")
		}
	case len(rest) == 1 && rest[0] == "issues" && r.Method == http.MethodPost:
		s.issues++
		writeJSON(w, 201, map[string]any{"iid": s.issues, "web_url": fmt.Sprintf("%s/demo/-/issues/%d", s.public, s.issues)})
	case len(rest) == 1 && rest[0] == "search":
		term := strings.ToLower(q.Get("search"))
		c := s.resolve("main")
		out := []map[string]string{}
		for path, data := range c.Files {
			if i := strings.Index(strings.ToLower(string(data)), term); term != "" && i >= 0 {
				from := max(0, i-60)
				out = append(out, map[string]string{"path": path, "data": string(data[from:min(len(data), i+120)])})
			}
		}
		writeJSON(w, 200, out)
	default:
		fail(w, 404, "404 Not Found")
	}
}

func userID(login string) int {
	h := sha1.Sum([]byte(login)) //nolint:gosec
	return int(h[0])<<16 | int(h[1])<<8 | int(h[2])
}

func (s *server) resolve(ref string) *commit {
	if id, ok := s.branches[ref]; ok {
		return s.commits[id]
	}
	return s.commits[ref]
}

// touches reports whether c changed anything at or under path compared to parent.
func touches(c, parent *commit, path string) bool {
	under := func(p string) bool { return p == path || strings.HasPrefix(p, path+"/") }
	for p, d := range c.Files {
		if !under(p) {
			continue
		}
		if parent == nil {
			return true
		}
		if pd, ok := parent.Files[p]; !ok || !bytes.Equal(pd, d) {
			return true
		}
	}
	if parent != nil {
		for p := range parent.Files {
			if _, ok := c.Files[p]; under(p) && !ok {
				return true
			}
		}
	}
	return false
}

func (s *server) commit(w http.ResponseWriter, r *http.Request, login string) {
	var in struct {
		Branch  string `json:"branch"`
		Message string `json:"commit_message"`
		Actions []struct {
			Action   string `json:"action"`
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		} `json:"actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid body")
		return
	}
	head := s.resolve(in.Branch)
	if head == nil {
		fail(w, 400, "You can only create or edit files when you are on a branch")
		return
	}
	files := map[string][]byte{}
	for k, v := range head.Files {
		files[k] = v
	}
	var added, modified, removed []string
	for _, a := range in.Actions {
		data := []byte(a.Content)
		if a.Encoding == "base64" {
			var err error
			if data, err = base64.StdEncoding.DecodeString(a.Content); err != nil {
				fail(w, 400, "invalid base64")
				return
			}
		}
		_, exists := files[a.FilePath]
		switch a.Action {
		case "create":
			if exists {
				fail(w, 400, "A file with this name already exists")
				return
			}
			files[a.FilePath] = data
			added = append(added, a.FilePath)
		case "update":
			if !exists {
				fail(w, 400, "A file with this name doesn't exist")
				return
			}
			files[a.FilePath] = data
			modified = append(modified, a.FilePath)
		case "delete":
			if !exists {
				fail(w, 400, "A file with this name doesn't exist")
				return
			}
			delete(files, a.FilePath)
			removed = append(removed, a.FilePath)
		default:
			fail(w, 400, "unknown action "+a.Action)
			return
		}
	}
	c := &commit{ID: newID(), Parent: head.ID, Message: in.Message, Author: login, Time: time.Now(), Files: files}
	s.commits[c.ID] = c
	s.branches[in.Branch] = c.ID
	writeJSON(w, 201, map[string]any{"id": c.ID, "message": c.Message})
	go s.push(in.Branch, login, c, added, modified, removed)
}

// push delivers a GitLab "Push Hook" to Hammurapi, like a project webhook.
func (s *server) push(branch, login string, c *commit, added, modified, removed []string) {
	if s.webhook == "" {
		return
	}
	nz := func(x []string) []string {
		if x == nil {
			return []string{}
		}
		return x
	}
	body, _ := json.Marshal(map[string]any{
		"object_kind": "push", "ref": "refs/heads/" + branch, "after": c.ID, "user_username": login,
		"commits": []map[string]any{{"id": c.ID, "message": c.Message, "timestamp": c.Time.Format(time.RFC3339),
			"added": nz(added), "modified": nz(modified), "removed": nz(removed)}},
	})
	req, _ := http.NewRequest(http.MethodPost, s.webhook, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", s.secret)
	req.Header.Set("X-Gitlab-Event-UUID", newID())
	for attempt := 0; attempt < 5; attempt++ {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			log.Printf("webhook %s: %s", s.webhook, resp.Status)
		} else {
			log.Printf("webhook %s: %v", s.webhook, err)
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
}
