// Command fakegitlab is an in-memory imitation of the parts of the GitLab API
// Hammurapi uses, for end-to-end tests and local demos without a real GitLab.
// It is not a GitLab replacement and must never be exposed publicly.
//
// It serves several projects (the specification repository and service
// repositories), OAuth sign-in, a bot token, branches, files, trees,
// multi-file commits, merge requests (3-way merge, diffs, rebase, notes,
// reviewers, approvals), merge base, tags, groups, pipelines, issues and code
// search, and sends project webhooks back to Hammurapi (push, tag push, merge
// request, note). For the closed cycle (FTR.HMR.CMN-0002) it also imitates CI —
// JUnit results of merge request branches are posted to /hooks/v1/ci-results —
// a deploy target and a Prometheus endpoint for metric dry runs.
//
// Configuration (env):
//
//	FAKE_ADDR            listen address (default :8929)
//	FAKE_PUBLIC_URL      URL browsers use to reach it (default http://localhost:8929)
//	FAKE_WEBHOOK_URL     Hammurapi git webhook, e.g. http://api:8080/hooks/v1/git
//	WEBHOOK_SECRET       sent as X-Gitlab-Token
//	FAKE_SPEC_REPO       the specification repository (default demo/specs)
//	FAKE_SEED_DIR        directory whose files seed the spec repository (e.g. rules/)
//	FAKE_SERVICE_REPOS   "fms/booking=FMS/CAR,fms/pricing=FMS/CAR": service repositories
//	                     seeded with a Go skeleton and catalog-info.yaml (system in Backstage terms)
//	FAKE_USERS           "login:Display Name,…" offered on the sign-in page
//	FAKE_GROUPS          "team-fleet:anna,oleg" groups for catalog owners
//	FAKE_BOT_TOKEN       token of the bot user hammurapi-bot
//	FAKE_CI_URL          Hammurapi CI results webhook (default derived from FAKE_WEBHOOK_URL)
//	FAKE_CI_SECRET       CI_RESULTS_SECRET used to sign CI results
//	FAKE_REFUSE_MERGE=1  refuse merges with 405 (to test provider refusals)
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // git ids
	"crypto/sha256"
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type commit struct {
	ID      string
	Parents []string
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
	Author    string
	State     string // opened | merged | closed
	MergeSHA  string
	Approvals []string
	Reviewers []int64
	BaseSHA   string // target head when the MR was opened (diff base)
}

type project struct {
	path     string
	branches map[string]string // branch → commit id
	tags     map[string]string
	mrs      map[int]*mr
	nextMR   int
	issues   int
}

type server struct {
	mu           sync.Mutex
	commits      map[string]*commit
	projects     map[string]*project
	users        map[string]string // login → name
	tokens       map[string]string // access token → login
	groups       map[string][]string
	public       string
	webhook      string
	secret       string
	ciURL        string
	ciSecret     string
	deploySecret string
	refuse       bool
	specRepo     string
}

func main() {
	s := &server{
		commits: map[string]*commit{}, projects: map[string]*project{}, users: map[string]string{}, tokens: map[string]string{},
		groups:  map[string][]string{},
		public:  strings.TrimRight(env("FAKE_PUBLIC_URL", "http://localhost:8929"), "/"),
		webhook: os.Getenv("FAKE_WEBHOOK_URL"), secret: os.Getenv("WEBHOOK_SECRET"),
		ciSecret: os.Getenv("FAKE_CI_SECRET"), refuse: os.Getenv("FAKE_REFUSE_MERGE") == "1",
		specRepo: env("FAKE_SPEC_REPO", "demo/specs"),
	}
	s.ciURL = env("FAKE_CI_URL", strings.Replace(s.webhook, "/hooks/v1/git", "/hooks/v1/ci-results", 1))
	for _, u := range strings.Split(env("FAKE_USERS", "admin:Admin,anna:Anna K.,oleg:Oleg D."), ",") {
		login, name, _ := strings.Cut(strings.TrimSpace(u), ":")
		if login != "" {
			s.users[login] = name
		}
	}
	s.users["hammurapi-bot"] = "Hammurapi Bot"
	if t := os.Getenv("FAKE_BOT_TOKEN"); t != "" {
		s.tokens[t] = "hammurapi-bot"
	}
	for _, g := range strings.Split(os.Getenv("FAKE_GROUPS"), ",") {
		name, members, ok := strings.Cut(strings.TrimSpace(g), ":")
		if ok {
			s.groups[name] = strings.Split(members, ";")
		}
	}
	spec := map[string][]byte{}
	if dir := os.Getenv("FAKE_SEED_DIR"); dir != "" {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(filepath.Dir(dir), p)
			if data, err := os.ReadFile(p); err == nil {
				spec[filepath.ToSlash(rel)] = data
			}
			return nil
		})
	}
	s.seed(s.specRepo, spec)
	for _, x := range strings.Split(os.Getenv("FAKE_SERVICE_REPOS"), ",") {
		repo, system, _ := strings.Cut(strings.TrimSpace(x), "=")
		if repo == "" {
			continue
		}
		name := repo[strings.LastIndex(repo, "/")+1:]
		files := map[string][]byte{
			"go.mod":    []byte("module example.com/" + name + "\n\ngo 1.22\n"),
			"main.go":   []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"" + name + "\") }\n"),
			"README.md": []byte("# " + name + "\n\nA demo service for Hammurapi.\n"),
		}
		if system != "" {
			files["catalog-info.yaml"] = []byte(fmt.Sprintf("apiVersion: backstage.io/v1alpha1\nkind: Component\nmetadata:\n  name: %s\nspec:\n  type: service\n  owner: user:anna\n  system: %s\n",
				name, strings.ToLower(strings.ReplaceAll(system, "/", "-"))))
		}
		s.seed(repo, files)
	}
	addr := env("FAKE_ADDR", ":8929")
	log.Printf("fakegitlab listening on %s: %d projects", addr, len(s.projects))
	log.Fatal(http.ListenAndServe(addr, s))
}

func (s *server) seed(repo string, files map[string][]byte) *project {
	root := &commit{ID: newID(), Message: "Initial commit", Author: "fakegitlab", Time: time.Now(), Files: files}
	s.commits[root.ID] = root
	p := &project{path: repo, branches: map[string]string{"main": root.ID}, tags: map[string]string{}, mrs: map[int]*mr{}, nextMR: 1}
	s.projects[repo] = p
	return p
}

func (s *server) project(path string) *project {
	if p, ok := s.projects[path]; ok {
		return p
	}
	return s.seed(path, map[string][]byte{"README.md": []byte("# " + path + "\n")})
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
	case r.URL.Path == "/fake/config" && r.Method == http.MethodPost:
		var in struct {
			DeploySecret string `json:"deploySecret"`
			CISecret     string `json:"ciSecret"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		if in.DeploySecret != "" {
			s.deploySecret = in.DeploySecret
		}
		if in.CISecret != "" {
			s.ciSecret = in.CISecret
		}
		s.mu.Unlock()
		w.WriteHeader(204)
	case r.URL.Path == "/fake/push" && r.Method == http.MethodPost:
		s.directPush(w, r)
	case r.URL.Path == "/fake/deploy" && r.Method == http.MethodPost:
		s.deployTarget(w, r)
	case r.URL.Path == "/fake/prometheus/api/v1/query":
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector",
			"result": []any{map[string]any{"metric": map[string]string{}, "value": []any{time.Now().Unix(), "42"}}}}})
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
		if l != "hammurapi-bot" {
			logins = append(logins, l)
		}
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

func userID(login string) int64 {
	h := sha1.Sum([]byte(login)) //nolint:gosec
	return int64(h[0])<<16 | int64(h[1])<<8 | int64(h[2])
}

func (s *server) mrJSON(p *project, m *mr) map[string]any {
	head := s.resolve(p, m.Source)
	sha := ""
	if head != nil {
		sha = head.ID
	}
	if m.State == "merged" {
		sha = m.MergeSHA
	}
	return map[string]any{"iid": m.IID, "title": m.Title, "state": m.State, "source_branch": m.Source, "target_branch": m.Target,
		"sha": sha, "merge_commit_sha": m.MergeSHA, "detailed_merge_status": "mergeable", "merge_status": "can_be_merged",
		"web_url": fmt.Sprintf("%s/%s/-/merge_requests/%d", s.public, p.path, m.IID)}
}

func (s *server) api(w http.ResponseWriter, r *http.Request, seg []string, login string) {
	q := r.URL.Query()
	switch {
	case len(seg) == 1 && seg[0] == "user":
		writeJSON(w, 200, map[string]any{"id": userID(login), "username": login, "name": s.users[login], "avatar_url": ""})
		return
	case len(seg) == 1 && seg[0] == "users":
		writeJSON(w, 200, []map[string]any{{"id": userID(q.Get("username")), "username": q.Get("username")}})
		return
	case len(seg) == 4 && seg[0] == "groups" && seg[2] == "members":
		out := []map[string]string{}
		for _, m := range s.groups[seg[1]] {
			out = append(out, map[string]string{"username": m})
		}
		writeJSON(w, 200, out)
		return
	}
	if len(seg) < 2 || seg[0] != "projects" {
		fail(w, 404, "404 Not Found")
		return
	}
	p := s.project(seg[1])
	rest := seg[2:]
	switch {
	case len(rest) == 0:
		writeJSON(w, 200, map[string]any{"path_with_namespace": p.path, "default_branch": "main"})
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "branches" && r.Method == http.MethodGet:
		id, ok := p.branches[rest[2]]
		if !ok {
			fail(w, 404, "404 Branch Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"name": rest[2], "commit": map[string]string{"id": id}})
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "branches" && r.Method == http.MethodPost:
		name, ref := q.Get("branch"), q.Get("ref")
		if _, exists := p.branches[name]; exists {
			fail(w, 400, "Branch already exists")
			return
		}
		c := s.resolve(p, ref)
		if c == nil {
			fail(w, 400, "Invalid reference name")
			return
		}
		p.branches[name] = c.ID
		writeJSON(w, 201, map[string]any{"name": name, "commit": map[string]string{"id": c.ID}})
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "branches" && r.Method == http.MethodDelete:
		if _, ok := p.branches[rest[2]]; !ok {
			fail(w, 404, "404 Branch Not Found")
			return
		}
		delete(p.branches, rest[2])
		w.WriteHeader(204)
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "files":
		c := s.resolve(p, q.Get("ref"))
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
		c := s.resolve(p, q.Get("ref"))
		if c == nil {
			fail(w, 404, "404 Tree Not Found")
			return
		}
		prefix := strings.Trim(q.Get("path"), "/")
		out := []map[string]string{}
		for path, data := range c.Files {
			if prefix == "" || strings.HasPrefix(path, prefix+"/") {
				out = append(out, map[string]string{"id": blobID(data), "path": path, "type": "blob", "name": filepath.Base(path)})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["path"] < out[j]["path"] })
		if page, _ := strconv.Atoi(q.Get("page")); page > 1 {
			out = []map[string]string{}
		}
		writeJSON(w, 200, out)
	case len(rest) == 4 && rest[0] == "repository" && rest[1] == "blobs" && rest[3] == "raw":
		// Blobs are found by id in any commit (FTR.HMR.CMN-0005 index of documents).
		for _, c := range s.commits {
			for _, data := range c.Files {
				if blobID(data) == rest[2] {
					_, _ = w.Write(data)
					return
				}
			}
		}
		fail(w, 404, "404 Blob Not Found")
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "commits" && r.Method == http.MethodPost:
		s.commit(w, r, p, login)
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "commits" && r.Method == http.MethodGet:
		c := s.resolve(p, q.Get("ref_name"))
		path := strings.Trim(q.Get("path"), "/")
		out := []map[string]string{}
		for c != nil && len(out) < 1 {
			parent := s.parent(c)
			if path == "" || touches(c, parent, path) {
				out = append(out, map[string]string{"id": c.ID, "message": c.Message})
			}
			c = parent
		}
		writeJSON(w, 200, out)
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "commits" && r.Method == http.MethodGet:
		c := s.resolve(p, rest[2])
		if c == nil {
			fail(w, 404, "404 Commit Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"id": c.ID, "parent_ids": c.Parents, "message": c.Message})
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "merge_base":
		refs := q["refs[]"]
		if len(refs) != 2 {
			fail(w, 400, "two refs required")
			return
		}
		a, b := s.resolve(p, refs[0]), s.resolve(p, refs[1])
		if a == nil || b == nil {
			fail(w, 404, "404 Not Found")
			return
		}
		base := s.mergeBase(a, b)
		if base == nil {
			fail(w, 404, "404 Not Found")
			return
		}
		writeJSON(w, 200, map[string]string{"id": base.ID})
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "tags" && r.Method == http.MethodPost:
		c := s.resolve(p, q.Get("ref"))
		if c == nil {
			fail(w, 400, "Target is invalid")
			return
		}
		p.tags[q.Get("tag_name")] = c.ID
		writeJSON(w, 201, map[string]any{"name": q.Get("tag_name"), "target": c.ID})
		go s.hook("Tag Push Hook", map[string]any{"object_kind": "tag_push", "ref": "refs/tags/" + q.Get("tag_name"), "after": c.ID,
			"checkout_sha": c.ID, "user_username": login, "project": map[string]string{"path_with_namespace": p.path}})
	case len(rest) == 1 && rest[0] == "merge_requests" && r.Method == http.MethodPost:
		var in struct {
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			Title        string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, m := range p.mrs {
			if m.Source == in.SourceBranch && m.State == "opened" {
				fail(w, 409, "Another open merge request already exists for this source branch")
				return
			}
		}
		m := &mr{IID: p.nextMR, Source: in.SourceBranch, Target: in.TargetBranch, Title: in.Title, Author: login, State: "opened",
			BaseSHA: p.branches[in.TargetBranch]}
		p.mrs[m.IID] = m
		p.nextMR++
		writeJSON(w, 201, s.mrJSON(p, m))
		s.mrHook(p, m, "open", login)
	case len(rest) >= 2 && rest[0] == "merge_requests":
		iid, _ := strconv.Atoi(rest[1])
		m, ok := p.mrs[iid]
		if !ok {
			fail(w, 404, "404 Not Found")
			return
		}
		switch {
		case len(rest) == 2 && r.Method == http.MethodGet:
			writeJSON(w, 200, s.mrJSON(p, m))
		case len(rest) == 3 && rest[2] == "merge":
			s.merge(w, p, m, login)
		case len(rest) == 3 && rest[2] == "rebase":
			writeJSON(w, 202, map[string]any{"rebase_in_progress": false})
		case len(rest) == 3 && rest[2] == "approve":
			m.Approvals = append(m.Approvals, login)
			writeJSON(w, 201, map[string]any{"iid": iid})
			s.mrHook(p, m, "approved", login)
		case len(rest) == 3 && rest[2] == "notes" && r.Method == http.MethodPost:
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			writeJSON(w, 201, map[string]any{"id": 1, "body": in.Body})
			go s.hook("Note Hook", map[string]any{"object_kind": "note", "user": map[string]string{"username": login},
				"project":           map[string]string{"path_with_namespace": p.path},
				"object_attributes": map[string]string{"note": in.Body, "noteable_type": "MergeRequest"}, "merge_request": map[string]int{"iid": iid}})
		case len(rest) == 3 && rest[2] == "diffs":
			if page, _ := strconv.Atoi(q.Get("page")); page > 1 {
				writeJSON(w, 200, []any{})
				return
			}
			writeJSON(w, 200, s.diffs(p, m))
		case len(rest) == 2 && r.Method == http.MethodPut:
			var in struct {
				StateEvent  string  `json:"state_event"`
				ReviewerIDs []int64 `json:"reviewer_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.StateEvent == "close" && m.State == "opened" {
				m.State = "closed"
				s.mrHook(p, m, "close", login)
			}
			if in.ReviewerIDs != nil {
				m.Reviewers = in.ReviewerIDs
			}
			writeJSON(w, 200, s.mrJSON(p, m))
		default:
			fail(w, 404, "404 Not Found")
		}
	case len(rest) == 1 && rest[0] == "pipeline" && r.Method == http.MethodPost:
		var in struct {
			Ref       string `json:"ref"`
			Variables []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		vars := map[string]string{}
		for _, v := range in.Variables {
			vars[v.Key] = v.Value
		}
		id := time.Now().UnixNano() % 1000000
		u := fmt.Sprintf("%s/%s/-/pipelines/%d", s.public, p.path, id)
		writeJSON(w, 201, map[string]any{"id": id, "web_url": u})
		if vars["callback_url"] != "" && vars["run_id"] != "" && vars["dryRun"] != "true" {
			go s.deployResult(vars["callback_url"], vars["run_id"], vars["service"], vars["environment"], in.Ref, u)
		}
	case len(rest) == 1 && rest[0] == "issues" && r.Method == http.MethodPost:
		p.issues++
		writeJSON(w, 201, map[string]any{"iid": p.issues, "web_url": fmt.Sprintf("%s/%s/-/issues/%d", s.public, p.path, p.issues)})
	case len(rest) == 1 && rest[0] == "search":
		term := strings.ToLower(q.Get("search"))
		c := s.resolve(p, "main")
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

func (s *server) resolve(p *project, ref string) *commit {
	if id, ok := p.branches[ref]; ok {
		return s.commits[id]
	}
	if id, ok := p.tags[ref]; ok {
		return s.commits[id]
	}
	return s.commits[ref]
}

func (s *server) parent(c *commit) *commit {
	if len(c.Parents) == 0 {
		return nil
	}
	return s.commits[c.Parents[0]]
}

func (s *server) ancestors(c *commit) map[string]int {
	out := map[string]int{}
	queue := []*commit{c}
	depth := 0
	for len(queue) > 0 && depth < 10000 {
		var next []*commit
		for _, x := range queue {
			if _, ok := out[x.ID]; ok {
				continue
			}
			out[x.ID] = depth
			for _, pid := range x.Parents {
				if pc := s.commits[pid]; pc != nil {
					next = append(next, pc)
				}
			}
		}
		queue = next
		depth++
	}
	return out
}

func (s *server) mergeBase(a, b *commit) *commit {
	ia := s.ancestors(a)
	ib := s.ancestors(b)
	var best *commit
	bestDepth := 1 << 30
	for id, d := range ib {
		if _, ok := ia[id]; ok && d < bestDepth {
			best, bestDepth = s.commits[id], d
		}
	}
	return best
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

func (s *server) diffs(p *project, m *mr) []map[string]any {
	head := s.resolve(p, m.Source)
	if m.State == "merged" {
		head = s.commits[m.MergeSHA]
	}
	target := s.resolve(p, m.Target)
	var base *commit
	if m.State == "merged" && head != nil {
		base = s.parent(head)
	} else if head != nil && target != nil {
		base = s.mergeBase(head, target)
	}
	out := []map[string]any{}
	if head == nil || base == nil {
		return out
	}
	for path, d := range head.Files {
		bd, ok := base.Files[path]
		switch {
		case !ok:
			out = append(out, map[string]any{"new_path": path, "old_path": path, "new_file": true})
		case !bytes.Equal(bd, d):
			out = append(out, map[string]any{"new_path": path, "old_path": path})
		}
	}
	for path := range base.Files {
		if _, ok := head.Files[path]; !ok {
			out = append(out, map[string]any{"new_path": path, "old_path": path, "deleted_file": true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["new_path"].(string) < out[j]["new_path"].(string) })
	return out
}

// merge performs a 3-way merge: target + changes of the source since the merge base.
func (s *server) merge(w http.ResponseWriter, p *project, m *mr, login string) {
	if s.refuse {
		fail(w, 405, "Method Not Allowed: branch is protected")
		return
	}
	if m.State != "opened" {
		fail(w, 405, "Merge request is not open")
		return
	}
	src, dst := s.resolve(p, m.Source), s.resolve(p, m.Target)
	if src == nil || dst == nil {
		fail(w, 406, "Source branch does not exist")
		return
	}
	base := s.mergeBase(src, dst)
	files := map[string][]byte{}
	for k, v := range dst.Files {
		files[k] = v
	}
	if base != nil {
		for path, d := range src.Files {
			if bd, ok := base.Files[path]; !ok || !bytes.Equal(bd, d) {
				files[path] = d
			}
		}
		for path := range base.Files {
			if _, ok := src.Files[path]; !ok {
				delete(files, path)
			}
		}
	} else {
		for k, v := range src.Files {
			files[k] = v
		}
	}
	c := &commit{ID: newID(), Parents: []string{dst.ID, src.ID}, Message: "Merge branch '" + m.Source + "'", Author: login, Time: time.Now(), Files: files}
	s.commits[c.ID] = c
	p.branches[m.Target] = c.ID
	m.State, m.MergeSHA = "merged", c.ID
	delete(p.branches, m.Source) // "delete source branch" like the default MR option
	writeJSON(w, 200, s.mrJSON(p, m))
	s.mrHook(p, m, "merge", login)
	go s.pushHook(p.path, m.Target, login, c, nil, nil, nil)
}

// directPush imitates a git push past Hammurapi (FTR.HMR.CMN-0005 demo): files are
// committed straight to a branch and the push webhook is sent.
func (s *server) directPush(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Project string            `json:"project"`
		Branch  string            `json:"branch"`
		Author  string            `json:"author"`
		Message string            `json:"message"`
		Files   map[string]string `json:"files"`
		Delete  []string          `json:"delete"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "invalid body")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.projects[in.Project]
	if p == nil {
		fail(w, 404, "404 Project Not Found")
		return
	}
	head := s.resolve(p, in.Branch)
	if head == nil {
		fail(w, 404, "404 Branch Not Found")
		return
	}
	files := map[string][]byte{}
	for k, v := range head.Files {
		files[k] = v
	}
	var added, modified, removed []string
	for path, content := range in.Files {
		if _, ok := files[path]; ok {
			modified = append(modified, path)
		} else {
			added = append(added, path)
		}
		files[path] = []byte(content)
	}
	for _, path := range in.Delete {
		if _, ok := files[path]; ok {
			delete(files, path)
			removed = append(removed, path)
		}
	}
	author := in.Author
	if author == "" {
		author = "admin"
	}
	c := &commit{ID: newID(), Parents: []string{head.ID}, Message: in.Message, Author: author, Time: time.Now(), Files: files}
	s.commits[c.ID] = c
	p.branches[in.Branch] = c.ID
	writeJSON(w, 201, map[string]any{"id": c.ID})
	go s.pushHook(p.path, in.Branch, author, c, added, modified, removed)
}

func (s *server) commit(w http.ResponseWriter, r *http.Request, p *project, login string) {
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
	head := s.resolve(p, in.Branch)
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
	c := &commit{ID: newID(), Parents: []string{head.ID}, Message: in.Message, Author: login, Time: time.Now(), Files: files}
	s.commits[c.ID] = c
	p.branches[in.Branch] = c.ID
	writeJSON(w, 201, map[string]any{"id": c.ID, "message": c.Message})
	go s.pushHook(p.path, in.Branch, login, c, added, modified, removed)
	for _, m := range p.mrs {
		if m.Source == in.Branch && m.State == "opened" {
			s.mrHook(p, m, "update", login)
		}
	}
}

// ─── Webhooks to Hammurapi ───────────────────────────────────────────

func (s *server) hook(event string, payload map[string]any) {
	if s.webhook == "" {
		return
	}
	body, _ := json.Marshal(payload)
	post(s.webhook, body, map[string]string{"X-Gitlab-Event": event, "X-Gitlab-Token": s.secret, "X-Gitlab-Event-UUID": newID()})
}

func post(u string, body []byte, headers map[string]string) {
	for attempt := 0; attempt < 5; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 300 || resp.StatusCode == 404 {
				return
			}
			log.Printf("POST %s: %s", u, resp.Status)
		} else {
			log.Printf("POST %s: %v", u, err)
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
}

// pushHook delivers a GitLab "Push Hook" like a project webhook.
func (s *server) pushHook(repo, branch, login string, c *commit, added, modified, removed []string) {
	nz := func(x []string) []string {
		if x == nil {
			return []string{}
		}
		return x
	}
	s.hook("Push Hook", map[string]any{
		"object_kind": "push", "ref": "refs/heads/" + branch, "after": c.ID, "user_username": login,
		"project": map[string]string{"path_with_namespace": repo},
		"commits": []map[string]any{{"id": c.ID, "message": c.Message, "timestamp": c.Time.Format(time.RFC3339),
			"added": nz(added), "modified": nz(modified), "removed": nz(removed)}},
	})
}

// mrHook delivers a "Merge Request Hook" (called with s.mu held) and, for
// service repositories, imitates the CI of the MR branch.
func (s *server) mrHook(p *project, m *mr, action, login string) {
	j := s.mrJSON(p, m)
	head, _ := j["sha"].(string)
	payload := map[string]any{"object_kind": "merge_request", "user": map[string]string{"username": login},
		"project": map[string]string{"path_with_namespace": p.path},
		"object_attributes": map[string]any{"iid": m.IID, "title": m.Title, "action": action, "source_branch": m.Source, "target_branch": m.Target,
			"url": j["web_url"], "merge_commit_sha": m.MergeSHA, "last_commit": map[string]string{"id": head}}}
	var files map[string][]byte
	if c := s.resolve(p, m.Source); c != nil {
		files = c.Files
	}
	go func() {
		s.hook("Merge Request Hook", payload)
		if (action == "open" || action == "update") && p.path != s.specRepo && head != "" {
			s.ci(p.path, head, m.Source, files)
		}
	}()
}

var testIDRe = regexp.MustCompile(`Test([A-Z]+)(\d{2,})_`)

// ci posts JUnit results for the test functions found in the branch.
func (s *server) ci(repo, sha, branch string, files map[string][]byte) {
	s.mu.Lock()
	secret, u := s.ciSecret, s.ciURL
	s.mu.Unlock()
	if secret == "" || u == "" {
		return
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><testsuites><testsuite name="` + html.EscapeString(repo) + `">`)
	var names []string
	for path, data := range files {
		if strings.HasSuffix(path, "_test.go") {
			for _, m := range testIDRe.FindAllStringSubmatch(string(data), -1) {
				names = append(names, fmt.Sprintf("Test%s%s", m[1], m[2]))
			}
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, `<testcase classname="%s" name="%s" time="0.01"/>`, html.EscapeString(repo), html.EscapeString(n))
	}
	b.WriteString(`</testsuite></testsuites>`)
	body, _ := json.Marshal(map[string]string{"repo": repo, "sha": sha, "branch": branch, "environment": "ci",
		"pipelineUrl": s.public + "/" + repo + "/-/pipelines/1", "junit": base64.StdEncoding.EncodeToString([]byte(b.String()))})
	post(u, body, signed(secret, body))
}

func signed(secret string, body []byte) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return map[string]string{"X-Hammurapi-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		"X-Hammurapi-Timestamp": strconv.FormatInt(time.Now().Unix(), 10)}
}

// deployTarget is a deploy webhook target (type "webhook" in the admin panel).
func (s *server) deployTarget(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RunID       string            `json:"runId"`
		Service     string            `json:"service"`
		Environment string            `json:"environment"`
		Ref         string            `json:"ref"`
		CallbackURL string            `json:"callbackUrl"`
		Params      map[string]string `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	w.WriteHeader(http.StatusAccepted)
	if in.CallbackURL != "" && in.Params["dryRun"] != "true" {
		go s.deployResult(in.CallbackURL, in.RunID, in.Service, in.Environment, in.Ref, s.public+"/deploys/"+in.RunID)
	}
}

func (s *server) deployResult(callback, runID, service, env, ref, runURL string) {
	s.mu.Lock()
	secret := s.deploySecret
	s.mu.Unlock()
	if secret == "" {
		log.Printf("deploy of %s: no deploy secret configured (POST /fake/config)", service)
		return
	}
	for _, status := range []string{"started", "success"} {
		time.Sleep(time.Second)
		body, _ := json.Marshal(map[string]string{"runId": runID, "service": service, "environment": env, "ref": ref, "status": status, "runUrl": runURL})
		post(callback, body, signed(secret, body))
	}
}
