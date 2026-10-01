package approvals

import (
	"context"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Repository reads the approvals queue.
type Repository struct{ q postgres.Querier }

// NewRepository creates a repository.
func NewRepository(q postgres.Querier) *Repository { return &Repository{q: q} }

// Pending lists in_review gates the user approves by expert kind, in domains
// with approval enabled, longest waiting first.
func (r *Repository) Pending(ctx context.Context, userID string, page httpx.Page) ([]Pending, error) {
	args := []any{userID, page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` AND (g.submitted_at, g.id::text) > ($3, $4)`
	}
	rows, err := r.q.Query(ctx, `
		SELECT f.unique_id, f.title, d.key, s.key, g.area, g.submitted_at, g.id::text,
			(SELECT u.display_name FROM gate_events e JOIN users u ON u.id = e.actor_id
			 WHERE e.gate_id = g.id AND e.event_type = 'submitted' ORDER BY e.created_at DESC LIMIT 1)
		FROM gates g
		JOIN features f ON f.id = g.feature_id
		JOIN systems  s ON s.id = f.system_id
		JOIN domains  d ON d.id = s.domain_id
		WHERE g.status = 'in_review'
		  AND g.deleted_at IS NULL
		  AND f.phase = 'spec'
		  AND d.approval_required
		  AND EXISTS (SELECT 1 FROM domain_experts e WHERE e.domain_id = d.id AND e.user_id = $1
		      AND e.kind = CASE WHEN g.area IN ('product','design') THEN 'product'::expert_kind ELSE 'technical'::expert_kind END)`+cond+`
		ORDER BY g.submitted_at ASC, g.id::text ASC
		LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.UniqueID, &p.Title, &p.Domain, &p.System, &p.Area, &p.SubmittedAt, &p.GateID, &p.SubmittedBy); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
