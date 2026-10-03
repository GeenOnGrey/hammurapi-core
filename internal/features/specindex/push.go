package specindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// maxFastCommits: larger pushes are left to the check, whose comparison by
// blob SHA catches up with them (tech spec §5).
const maxFastCommits = 20

// SpecsPushed handles a push to the specification repository: on the default
// branch with changes under specs/ it updates the documents of known features
// right away (R17) and queues a check that compares the index with the tree.
func (s *Service) SpecsPushed(ctx context.Context, ev *git.PushEvent) error {
	if ev == nil || ev.Branch != s.cfg.DefaultBranch {
		return nil
	}
	final := map[string]bool{} // path → present after the push
	for _, c := range ev.Commits {
		for _, p := range append(append([]string{}, c.Added...), c.Modified...) {
			if strings.HasPrefix(p, "specs/") {
				final[p] = true
			}
		}
		for _, p := range c.Removed {
			if strings.HasPrefix(p, "specs/") {
				final[p] = false
			}
		}
	}
	if len(final) == 0 {
		return nil
	}
	if _, err := s.Enqueue(ctx, s.pool, TriggerPush, nil); err != nil {
		return err
	}
	if len(ev.Commits) > maxFastCommits {
		return nil
	}
	token, err := s.git.BotToken(ctx)
	if err != nil {
		return err
	}
	ref := ev.After
	if ref == "" {
		ref = s.cfg.DefaultBranch
	}
	changed := false
	for p, present := range final {
		loc, ok := ParsePath(p)
		if !ok {
			continue
		}
		var indexable bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE unique_id = $1 AND phase <> 'deleted')`,
			loc.Folder).Scan(&indexable); err != nil {
			return err
		}
		if !indexable {
			continue // new folders are indexed by the check
		}
		if !present {
			table, col := "spec_files", "path"
			if loc.IsDocument() {
				table = "spec_documents"
			}
			if _, err := s.pool.Exec(ctx, `DELETE FROM `+table+` WHERE `+col+` = $1`, p); err != nil {
				return err
			}
			changed = true
			continue
		}
		f, err := s.git.GetFile(ctx, token, ref, p)
		if errors.Is(err, git.ErrNotFound) {
			continue // removed by a later push; the check catches up
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if loc.IsDocument() {
			err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				return s.writeDocument(ctx, tx, loc, f.BlobSHA, ref, string(f.Content))
			})
		} else {
			err = writeFile(ctx, s.pool, loc, p, f.BlobSHA, int64(len(f.Content)))
		}
		if err != nil {
			return err
		}
		changed = true
	}
	if changed && s.events != nil {
		s.events.Publish(ctx, events.Event{Type: EventIndexUpdated, Data: map[string]any{"commit": ref}})
	}
	return nil
}

// FocusItem is an element of the "Specification" group of "In focus" (R10).
type FocusItem struct {
	Kind         string    `json:"kind"` // spec
	Key          string    `json:"key"`
	Title        string    `json:"title"`
	Action       string    `json:"action"` // missing_catalog | bad_id | missing_parent | deleted
	WaitingSince time.Time `json:"waitingSince"`
	Hint         *string   `json:"hint,omitempty"`
}

// Focus lists the open indexing problems for global administrators; missing
// domains and systems are grouped by key.
func (s *Service) Focus(ctx context.Context) ([]FocusItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, path, COALESCE(feature_key, ''), details, first_seen_at
		FROM spec_index_issues WHERE resolved_at IS NULL ORDER BY first_seen_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FocusItem{}
	catalog := map[string]int{} // "LOG/DLV" → index in out
	for rows.Next() {
		var kind, path, key string
		var raw []byte
		var since time.Time
		if err := rows.Scan(&kind, &path, &key, &raw, &since); err != nil {
			return nil, err
		}
		var d map[string]any
		_ = json.Unmarshal(raw, &d)
		str := func(k string) string { v, _ := d[k].(string); return v }
		switch kind {
		case KindMissingDomain, KindMissingSystem:
			ck := str("domain") + "/" + str("system")
			if i, ok := catalog[ck]; ok {
				h := *out[i].Hint + ", " + key
				out[i].Hint = &h
				continue
			}
			h := key
			catalog[ck] = len(out)
			out = append(out, FocusItem{Kind: "spec", Key: ck, Title: ck, Action: "missing_catalog", WaitingSince: since, Hint: &h})
		case KindOldFormat, KindBadFormat, KindKeyPathMismatch:
			h := kind
			if sug := str("suggested"); sug != "" {
				h = kind + ": " + sug
			}
			out = append(out, FocusItem{Kind: "spec", Key: key, Title: path, Action: "bad_id", WaitingSince: since, Hint: &h})
		case KindMissingParent:
			h := str("parent")
			out = append(out, FocusItem{Kind: "spec", Key: key, Title: path, Action: "missing_parent", WaitingSince: since, Hint: &h})
		case KindDeleted:
			out = append(out, FocusItem{Kind: "spec", Key: key, Title: path, Action: "deleted", WaitingSince: since})
		}
	}
	return out, rows.Err()
}
