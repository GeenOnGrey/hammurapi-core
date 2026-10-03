package agentcfg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Skills live in the specification repository (R12).
const (
	SkillsDir  = "agent/skills"
	SkillsYAML = "agent/skills.yaml"
)

// Limits of an uploaded skill (tech spec §2.3).
const (
	maxSkillZip      = 5 << 20
	maxSkillUnpacked = 20 << 20
	maxSkillFiles    = 200
)

var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// SkillMeta is a skill of a snapshot.
type SkillMeta struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Scenarios   []agent.Scenario `json:"scenarios"`
}

// Skill is a skill as the API lists it.
type Skill struct {
	SkillMeta
	Status string       `json:"status"` // active | pending_add | pending_change | pending_delete
	Change *SkillChange `json:"change,omitempty"`
}

// SkillChange is an open skill PR.
type SkillChange struct {
	ID        uuid.UUID `json:"id"`
	Skill     string    `json:"skill"`
	Kind      string    `json:"kind"` // add | change | delete
	PRURL     string    `json:"prUrl"`
	PRNumber  int       `json:"prNumber"`
	AuthorID  uuid.UUID `json:"authorId"`
	Author    string    `json:"author"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	Details   struct {
		Scenarios   []agent.Scenario `json:"scenarios"`
		Description string           `json:"description"`
	} `json:"details"`
}

// skillsConfig is /agent/skills.yaml: the binding of skills to scenarios.
type skillsConfig struct {
	Skills map[string]struct {
		Scenarios []agent.Scenario `yaml:"scenarios"`
	} `yaml:"skills"`
}

// SkillFile is a file of an uploaded skill, relative to its directory.
type SkillFile struct {
	Path    string
	Content []byte
}

// ParsedSkill is a validated upload.
type ParsedSkill struct {
	Name, Description string
	Files             []SkillFile
}

// ValidationError lists the reasons a skill was rejected (SK-06, SK-07).
func invalid(reasons ...string) error {
	return apperr.Unprocessable("skill_invalid", "the skill is not valid").With("details", reasons)
}

// ParseSkillMD validates the frontmatter of SKILL.md (Agent Skills format).
func ParseSkillMD(content []byte) (name, description string, err error) {
	s := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", "", invalid("SKILL.md has no YAML frontmatter")
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return "", "", invalid("SKILL.md frontmatter is not closed")
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(s[4:4+end]), &fm); err != nil {
		return "", "", invalid("SKILL.md frontmatter is not valid YAML: " + err.Error())
	}
	var reasons []string
	switch {
	case fm.Name == "":
		reasons = append(reasons, "name is required")
	case len(fm.Name) > 64 || !skillNameRe.MatchString(fm.Name):
		reasons = append(reasons, "name must be lower-case letters, digits and single dashes, at most 64 characters")
	}
	d := strings.TrimSpace(fm.Description)
	if d == "" || len(d) > 1024 {
		reasons = append(reasons, "description is required and at most 1024 characters")
	}
	if len(reasons) > 0 {
		return "", "", invalid(reasons...)
	}
	return fm.Name, d, nil
}

// ParseSkillZip validates an uploaded archive (R14): ≤ 5 MB packed, ≤ 20 MB
// unpacked, ≤ 200 files, no absolute paths, "..", or links; SKILL.md at the
// root or in exactly one top directory.
func ParseSkillZip(data []byte) (*ParsedSkill, error) {
	if len(data) > maxSkillZip {
		return nil, invalid("the archive is larger than 5 MB")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, invalid("not a zip archive")
	}
	var files []SkillFile
	var total int64
	tops := map[string]bool{}
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Mode().Type() != 0 {
			return nil, invalid("links and special files are not allowed: " + f.Name)
		}
		clean := path.Clean(name)
		if path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(name, "/../") {
			return nil, invalid("unsafe path: " + f.Name)
		}
		if len(files) >= maxSkillFiles {
			return nil, invalid("more than 200 files")
		}
		total += int64(f.UncompressedSize64)
		if total > maxSkillUnpacked {
			return nil, invalid("the unpacked skill is larger than 20 MB")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, invalid("cannot read " + f.Name)
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxSkillUnpacked+1))
		rc.Close()
		if err != nil || int64(len(b)) > maxSkillUnpacked {
			return nil, invalid("cannot read " + f.Name)
		}
		if strings.Contains(clean, "/") {
			tops[strings.SplitN(clean, "/", 2)[0]] = true
		} else {
			tops[""] = true
		}
		files = append(files, SkillFile{Path: clean, Content: b})
	}
	// SKILL.md at the root, or everything inside one directory.
	prefix := ""
	if !tops[""] {
		if len(tops) != 1 {
			return nil, invalid("the archive must contain one skill directory with SKILL.md")
		}
		for t := range tops {
			prefix = t + "/"
		}
	}
	var md []byte
	out := &ParsedSkill{}
	for _, f := range files {
		rel := strings.TrimPrefix(f.Path, prefix)
		if rel == "SKILL.md" {
			md = f.Content
		}
		out.Files = append(out.Files, SkillFile{Path: rel, Content: f.Content})
	}
	if md == nil {
		return nil, invalid("SKILL.md is missing")
	}
	if out.Name, out.Description, err = ParseSkillMD(md); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseSkillText builds a skill from the text of SKILL.md.
func ParseSkillText(md string) (*ParsedSkill, error) {
	if len(md) > maxSkillZip {
		return nil, invalid("SKILL.md is larger than 5 MB")
	}
	name, d, err := ParseSkillMD([]byte(md))
	if err != nil {
		return nil, err
	}
	return &ParsedSkill{Name: name, Description: d, Files: []SkillFile{{Path: "SKILL.md", Content: []byte(md)}}}, nil
}

// ─── listing ────────────────────────────────────────────────────────

type snapshot struct {
	Hash, CommitSHA, S3Key string
	Skills                 []SkillMeta
}

func (s *Service) latestSnapshot(ctx context.Context) (*snapshot, error) {
	var sn snapshot
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT hash, commit_sha, s3_key, skills FROM agent_skill_snapshots ORDER BY created_at DESC LIMIT 1`).
		Scan(&sn.Hash, &sn.CommitSHA, &sn.S3Key, &raw)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sn, json.Unmarshal(raw, &sn.Skills)
}

const changeSelect = `SELECT c.id, c.object_ref, c.details, COALESCE(c.pr_url,''), COALESCE(c.pr_number,0), c.actor_id, COALESCE(u.display_name,''),
	c.pr_state, c.created_at FROM agent_config_changes c LEFT JOIN users u ON u.id = c.actor_id`

func scanSkillChange(row pgx.Row) (*SkillChange, error) {
	var c SkillChange
	var details []byte
	var author *uuid.UUID
	if err := row.Scan(&c.ID, &c.Skill, &details, &c.PRURL, &c.PRNumber, &author, &c.Author, &c.State, &c.CreatedAt); err != nil {
		return nil, err
	}
	if author != nil {
		c.AuthorID = *author
	}
	var d struct {
		Kind        string           `json:"kind"`
		Scenarios   []agent.Scenario `json:"scenarios"`
		Description string           `json:"description"`
	}
	_ = json.Unmarshal(details, &d)
	c.Kind, c.Details.Scenarios, c.Details.Description = d.Kind, d.Scenarios, d.Description
	return &c, nil
}

func (s *Service) openSkillChanges(ctx context.Context) ([]SkillChange, error) {
	rows, err := s.pool.Query(ctx, changeSelect+` WHERE c.object_type = 'skill' AND c.pr_state = 'open' ORDER BY c.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillChange{}
	for rows.Next() {
		c, err := scanSkillChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SkillChanges lists the open skill changes.
func (s *Service) SkillChanges(ctx context.Context, p *domain.Principal) ([]SkillChange, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	return s.openSkillChanges(ctx)
}

// Skills lists the active skills and the pending changes (R13).
func (s *Service) Skills(ctx context.Context, p *domain.Principal) ([]Skill, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	sn, err := s.latestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	changes, err := s.openSkillChanges(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*Skill{}
	var order []string
	if sn != nil {
		for _, m := range sn.Skills {
			byName[m.Name] = &Skill{SkillMeta: m, Status: "active"}
			order = append(order, m.Name)
		}
	}
	for i := range changes {
		c := &changes[i]
		sk, ok := byName[c.Skill]
		if !ok {
			sk = &Skill{SkillMeta: SkillMeta{Name: c.Skill, Description: c.Details.Description, Scenarios: c.Details.Scenarios}}
			byName[c.Skill] = sk
			order = append(order, c.Skill)
		}
		sk.Status = "pending_" + c.Kind
		sk.Change = c
	}
	out := make([]Skill, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// ─── proposals (PRs) ────────────────────────────────────────────────

// readSkillsConfig reads /agent/skills.yaml at ref.
func (s *Service) readSkillsConfig(ctx context.Context, token, ref string) (skillsConfig, error) {
	cfg := skillsConfig{}
	f, err := s.git.GetFile(ctx, token, ref, SkillsYAML)
	if errors.Is(err, git.ErrNotFound) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(f.Content, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", SkillsYAML, err)
	}
	return cfg, nil
}

func renderSkillsConfig(cfg skillsConfig) []byte {
	names := make([]string, 0, len(cfg.Skills))
	for n := range cfg.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# Binding of agent skills to Hammurapi scenarios (PLT.HMR-0004 R12).\n# Changed through the Agent section; every change is a PR.\nskills:\n")
	if len(names) == 0 {
		return []byte(strings.TrimSuffix(b.String(), "\n") + " {}\n")
	}
	for _, n := range names {
		scs := make([]string, 0, len(cfg.Skills[n].Scenarios))
		for _, sc := range cfg.Skills[n].Scenarios {
			scs = append(scs, string(sc))
		}
		fmt.Fprintf(&b, "  %s:\n    scenarios: [%s]\n", n, strings.Join(scs, ", "))
	}
	return []byte(b.String())
}

func validScenarios(scs []agent.Scenario) error {
	for _, sc := range scs {
		if !sc.Valid() {
			return apperr.Unprocessable("invalid_scenario", "unknown scenario "+string(sc)).With("field", "scenarios")
		}
	}
	return nil
}

// propose creates a branch, a commit and a PR, and records the change.
func (s *Service) propose(ctx context.Context, p *domain.Principal, name, kind, title string, scs []agent.Scenario, description string,
	changes func(token string, cfg *skillsConfig) ([]git.FileChange, error)) (*SkillChange, error) {
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.readSkillsConfig(ctx, token, s.defaultBranch)
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	files, err := changes(token, &cfg)
	if err != nil {
		return nil, err
	}
	files = append(files, git.FileChange{Path: SkillsYAML, Content: renderSkillsConfig(cfg)})
	head, err := s.git.BranchHead(ctx, token, s.defaultBranch)
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	branch := fmt.Sprintf("agent-skills/%s-%d", name, time.Now().Unix())
	if err := s.git.CreateBranch(ctx, token, branch, head); err != nil {
		return nil, auth.MapGitError(err)
	}
	fail := func(cause error) (*SkillChange, error) {
		_ = s.git.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		return nil, cause
	}
	if _, err := s.git.Commit(ctx, token, branch, "agent skills: "+title, files); err != nil {
		return fail(auth.MapGitError(err))
	}
	pr, err := s.git.CreatePR(ctx, token, branch, s.defaultBranch, "Agent skills: "+title,
		"Proposed in Hammurapi → Administration → Agent → Skills. The agent sees the change after another global administrator approves it.")
	if err != nil {
		return fail(auth.MapGitError(err))
	}
	details, _ := json.Marshal(map[string]any{"kind": kind, "scenarios": scs, "description": description})
	id := uuid.New()
	_, err = s.pool.Exec(ctx, `INSERT INTO agent_config_changes (id, object_type, object_ref, action, summary, actor_id, pr_url, pr_number, branch_name, pr_state, details)
		VALUES ($1,'skill',$2,'propose',$3,$4,$5,$6,$7,'open',$8)`, id, name, "skill "+kind+": "+title, p.UserID, pr.URL, pr.Number, branch, details)
	if err != nil {
		_ = s.git.ClosePR(context.WithoutCancel(ctx), token, pr.Number)
		_ = s.git.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		if postgres.IsUniqueViolation(err) {
			return nil, apperr.Conflict("skill_change_open", "the skill already has an open change")
		}
		return nil, err
	}
	return scanSkillChange(s.pool.QueryRow(ctx, changeSelect+` WHERE c.id = $1`, id))
}

// nameTaken: an active skill or an open change with this name (SK-08).
func (s *Service) nameTaken(ctx context.Context, name string) (bool, error) {
	list, err := s.Skills(ctx, &domain.Principal{GlobalAdmin: true})
	if err != nil {
		return false, err
	}
	for _, sk := range list {
		if sk.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// AddSkill proposes a new skill (R13, R14).
func (s *Service) AddSkill(ctx context.Context, p *domain.Principal, sk *ParsedSkill, scs []agent.Scenario) (*SkillChange, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if err := validScenarios(scs); err != nil {
		return nil, err
	}
	taken, err := s.nameTaken(ctx, sk.Name)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, apperr.Conflict("skill_name_conflict", "a skill with this name already exists or is pending").With("field", "name")
	}
	return s.propose(ctx, p, sk.Name, "add", "add "+sk.Name, scs, sk.Description, func(_ string, cfg *skillsConfig) ([]git.FileChange, error) {
		if cfg.Skills == nil {
			cfg.Skills = map[string]struct {
				Scenarios []agent.Scenario `yaml:"scenarios"`
			}{}
		}
		cfg.Skills[sk.Name] = struct {
			Scenarios []agent.Scenario `yaml:"scenarios"`
		}{Scenarios: scs}
		files := make([]git.FileChange, 0, len(sk.Files))
		for _, f := range sk.Files {
			files = append(files, git.FileChange{Path: SkillsDir + "/" + sk.Name + "/" + f.Path, Content: f.Content})
		}
		return files, nil
	})
}

func (s *Service) activeSkill(ctx context.Context, name string) (*SkillMeta, error) {
	sn, err := s.latestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if sn != nil {
		for _, m := range sn.Skills {
			if m.Name == name {
				return &m, nil
			}
		}
	}
	return nil, apperr.NotFound("skill_not_found", "no active skill with this name")
}

// SetSkillScenarios proposes a new binding of a skill.
func (s *Service) SetSkillScenarios(ctx context.Context, p *domain.Principal, name string, scs []agent.Scenario) (*SkillChange, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if err := validScenarios(scs); err != nil {
		return nil, err
	}
	m, err := s.activeSkill(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.propose(ctx, p, name, "change", "scenarios of "+name, scs, m.Description, func(_ string, cfg *skillsConfig) ([]git.FileChange, error) {
		if cfg.Skills == nil {
			cfg.Skills = map[string]struct {
				Scenarios []agent.Scenario `yaml:"scenarios"`
			}{}
		}
		cfg.Skills[name] = struct {
			Scenarios []agent.Scenario `yaml:"scenarios"`
		}{Scenarios: scs}
		return nil, nil
	})
}

// DeleteSkill proposes removing a skill.
func (s *Service) DeleteSkill(ctx context.Context, p *domain.Principal, name string) (*SkillChange, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	m, err := s.activeSkill(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.propose(ctx, p, name, "delete", "delete "+name, m.Scenarios, m.Description, func(token string, cfg *skillsConfig) ([]git.FileChange, error) {
		delete(cfg.Skills, name)
		paths, err := s.git.ListFiles(ctx, token, s.defaultBranch, SkillsDir+"/"+name)
		if err != nil {
			return nil, auth.MapGitError(err)
		}
		files := make([]git.FileChange, 0, len(paths))
		for _, pth := range paths {
			files = append(files, git.FileChange{Path: pth, Delete: true})
		}
		return files, nil
	})
}

func (s *Service) skillChange(ctx context.Context, id uuid.UUID) (*SkillChange, string, error) {
	var branch string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(branch_name,'') FROM agent_config_changes WHERE id = $1 AND object_type = 'skill'`, id).Scan(&branch); err != nil {
		if postgres.IsNoRows(err) {
			return nil, "", apperr.NotFound("change_not_found", "skill change not found")
		}
		return nil, "", err
	}
	c, err := scanSkillChange(s.pool.QueryRow(ctx, changeSelect+` WHERE c.id = $1`, id))
	return c, branch, err
}

// ApproveSkill merges the PR with the approver's token. The author may
// approve only when the instance has a single global administrator (SK-04, SK-05).
func (s *Service) ApproveSkill(ctx context.Context, p *domain.Principal, id uuid.UUID) (*SkillChange, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	c, branch, err := s.skillChange(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.State != "open" {
		return nil, apperr.Conflict("change_closed", "the change is not open")
	}
	self := c.AuthorID == p.UserID
	if self {
		var admins int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_global_admin`).Scan(&admins); err != nil {
			return nil, err
		}
		if admins > 1 {
			return nil, apperr.Forbidden("own_change", "another global administrator must approve this change")
		}
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if !self {
		if err := s.git.ApprovePR(ctx, token, c.PRNumber); err != nil {
			slog.WarnContext(ctx, "provider approval failed", "pr", c.PRNumber, "err", err)
		}
	}
	if err := s.git.MergePR(ctx, token, c.PRNumber, "Agent skills: "+c.Skill); err != nil {
		return nil, auth.MapGitError(err)
	}
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE agent_config_changes SET pr_state = 'merged' WHERE id = $1`, id); err != nil {
			return err
		}
		summary := "skill " + c.Kind + " " + c.Skill + " approved"
		if self {
			summary += " by the author (the only global administrator)"
		}
		_, err := tx.Exec(ctx, `INSERT INTO agent_config_changes (object_type, object_ref, action, summary, actor_id, pr_url, pr_number, self_approved)
			VALUES ('skill',$1,'approve',$2,$3,$4,$5,$6)`, c.Skill, summary, p.UserID, c.PRURL, c.PRNumber, self)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := s.git.DeleteBranch(ctx, token, branch); err != nil {
		slog.WarnContext(ctx, "delete skills branch", "branch", branch, "err", err)
	}
	// The push webhook also triggers the sync; doing it now makes the skill
	// visible to the next sessions without waiting for the webhook.
	if _, err := s.SyncSnapshot(ctx, token); err != nil {
		slog.WarnContext(ctx, "skills snapshot after merge", "err", err)
	}
	c, _, err = s.skillChange(ctx, id)
	return c, err
}

// WithdrawSkill closes the author's PR without merging.
func (s *Service) WithdrawSkill(ctx context.Context, p *domain.Principal, id uuid.UUID) (*SkillChange, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	c, branch, err := s.skillChange(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.AuthorID != p.UserID {
		return nil, apperr.Forbidden("not_author", "only the author can withdraw a change")
	}
	if c.State != "open" {
		return nil, apperr.Conflict("change_closed", "the change is not open")
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if err := s.git.ClosePR(ctx, token, c.PRNumber); err != nil && !errors.Is(err, git.ErrNotFound) {
		return nil, auth.MapGitError(err)
	}
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE agent_config_changes SET pr_state = 'closed' WHERE id = $1`, id); err != nil {
			return err
		}
		return audit(ctx, tx, &p.UserID, "skill", c.Skill, "withdraw", "skill "+c.Kind+" "+c.Skill+" withdrawn")
	})
	if err != nil {
		return nil, err
	}
	_ = s.git.DeleteBranch(ctx, token, branch)
	c, _, err = s.skillChange(ctx, id)
	return c, err
}

// ─── snapshots (arch §5) ────────────────────────────────────────────

// SyncSnapshot builds the snapshot of /agent/skills at the default branch:
// a deterministic tar.gz stored in object storage by its sha256. An unchanged
// tree yields the same hash and nothing new is stored.
func (s *Service) SyncSnapshot(ctx context.Context, token string) (string, error) {
	head, err := s.git.BranchHead(ctx, token, s.defaultBranch)
	if err != nil {
		return "", err
	}
	paths, err := s.git.ListFiles(ctx, token, head, SkillsDir)
	if err != nil && !errors.Is(err, git.ErrNotFound) {
		return "", err
	}
	cfg, err := s.readSkillsConfig(ctx, token, head)
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	metas := map[string]*SkillMeta{}
	var names []string
	for _, pth := range paths {
		rel := strings.TrimPrefix(pth, SkillsDir+"/")
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) != 2 || !skillNameRe.MatchString(parts[0]) {
			continue
		}
		f, err := s.git.GetFile(ctx, token, head, pth)
		if err != nil {
			return "", err
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(f.Content)), Typeflag: tar.TypeReg,
			ModTime: time.Unix(0, 0)}); err != nil {
			return "", err
		}
		if _, err := tw.Write(f.Content); err != nil {
			return "", err
		}
		if parts[1] == "SKILL.md" {
			name, desc, err := ParseSkillMD(f.Content)
			if err != nil || name != parts[0] {
				slog.WarnContext(ctx, "skip invalid skill in the repository", "skill", parts[0])
				continue
			}
			metas[name] = &SkillMeta{Name: name, Description: desc, Scenarios: cfg.Skills[name].Scenarios}
			names = append(names, name)
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	hash := "sha256:" + hex.EncodeToString(sum[:])
	list := make([]SkillMeta, 0, len(names))
	for _, n := range names {
		if metas[n].Scenarios == nil {
			metas[n].Scenarios = []agent.Scenario{}
		}
		list = append(list, *metas[n])
	}
	raw, _ := json.Marshal(list)
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_skill_snapshots WHERE hash = $1)`, hash).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		// The archive holds only the skills' files; the scenario bindings live in
		// agent/skills.yaml, so a change of bindings keeps the hash — refresh them.
		_, err := s.pool.Exec(ctx, `UPDATE agent_skill_snapshots SET created_at = now(), commit_sha = $2, skills = $3 WHERE hash = $1`, hash, head, raw)
		return hash, err
	}
	key := "agent/skills/" + hex.EncodeToString(sum[:]) + ".tar.gz"
	if err := s.store.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "application/gzip"); err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO agent_skill_snapshots (hash, commit_sha, s3_key, skills) VALUES ($1,$2,$3,$4) ON CONFLICT (hash) DO NOTHING`,
		hash, head, key, raw)
	slog.InfoContext(ctx, "agent skills snapshot", "hash", hash, "skills", len(list), "commit", head)
	return hash, err
}

// SkillsChanged is called by the push processor for the specification
// repository; it rebuilds the snapshot when /agent/ changed on the default branch.
func (s *Service) SkillsChanged(ctx context.Context, branch string, paths []string) error {
	if branch != s.defaultBranch {
		return nil
	}
	for _, pth := range paths {
		if strings.HasPrefix(pth, "agent/") {
			token, err := s.git.BotToken(ctx)
			if err != nil {
				return err
			}
			_, err = s.SyncSnapshot(ctx, token)
			return err
		}
	}
	return nil
}

// SkillsBundle returns the archive of a snapshot (sent to the operator on demand, SK-10).
func (s *Service) SkillsBundle(ctx context.Context, hash string) ([]byte, error) {
	var key string
	if err := s.pool.QueryRow(ctx, `SELECT s3_key FROM agent_skill_snapshots WHERE hash = $1`, hash).Scan(&key); err != nil {
		return nil, err
	}
	rc, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 64<<20))
}

// skillsFor returns the snapshot hash and the skills bound to a scenario.
func (s *Service) skillsFor(ctx context.Context, sc agent.Scenario) (*agent.Skills, error) {
	sn, err := s.latestSnapshot(ctx)
	if err != nil || sn == nil {
		return nil, err
	}
	var names []string
	for _, m := range sn.Skills {
		for _, x := range m.Scenarios {
			if x == sc {
				names = append(names, m.Name)
			}
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	return &agent.Skills{Hash: sn.Hash, Names: names}, nil
}
