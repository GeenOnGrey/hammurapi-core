package agent

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Message is a chat message.
type Message struct {
	ID          uuid.UUID       `json:"id"`
	Role        string          `json:"role"` // user | agent
	Mode        string          `json:"mode"` // general | spec
	Feature     *string         `json:"feature"`
	Area        *domain.Area    `json:"area"`
	Content     string          `json:"content"`
	IsVoice     bool            `json:"isVoice"`
	Attachments []AttachmentRef `json:"attachments"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// AttachmentRef is an attachment shown with a message.
type AttachmentRef struct {
	ID       uuid.UUID `json:"id"`
	FileName string    `json:"fileName"`
	MimeType string    `json:"mimeType"`
}

// Repository persists chat messages and ACP session ids.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository creates a repository.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Insert stores a message.
func (r *Repository) Insert(ctx context.Context, q postgres.Querier, userID uuid.UUID, role, mode string, featureID *uuid.UUID, area *domain.Area, content string, isVoice bool) (uuid.UUID, time.Time, error) {
	var id uuid.UUID
	var at time.Time
	err := q.QueryRow(ctx, `INSERT INTO chat_messages (user_id, role, mode, feature_id, area, content, is_voice)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`, userID, role, mode, featureID, area, content, isVoice).Scan(&id, &at)
	return id, at, err
}

// History lists messages newest first.
func (r *Repository) History(ctx context.Context, userID uuid.UUID, page httpx.Page) ([]Message, error) {
	args := []any{userID, page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` AND (m.created_at, m.id::text) < ($3, $4)`
	}
	rows, err := r.pool.Query(ctx, `SELECT m.id, m.role, m.mode, f.unique_id, m.area, m.content, m.is_voice, m.created_at
		FROM chat_messages m LEFT JOIN features f ON f.id = m.feature_id
		WHERE m.user_id = $1`+cond+` ORDER BY m.created_at DESC, m.id::text DESC LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	var out []Message
	ids := []uuid.UUID{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Role, &m.Mode, &m.Feature, &m.Area, &m.Content, &m.IsVoice, &m.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		m.Attachments = []AttachmentRef{}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return out, err
	}
	arows, err := r.pool.Query(ctx, `SELECT message_id, id, file_name, mime_type FROM attachments WHERE message_id = ANY($1) ORDER BY created_at`, ids)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	byMsg := map[uuid.UUID][]AttachmentRef{}
	for arows.Next() {
		var mid uuid.UUID
		var a AttachmentRef
		if err := arows.Scan(&mid, &a.ID, &a.FileName, &a.MimeType); err != nil {
			return nil, err
		}
		byMsg[mid] = append(byMsg[mid], a)
	}
	for i := range out {
		if as, ok := byMsg[out[i].ID]; ok {
			out[i].Attachments = as
		}
	}
	return out, arows.Err()
}

// SessionID returns the last ACP session id of the user.
func (r *Repository) SessionID(ctx context.Context, userID uuid.UUID) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `SELECT session_id FROM agent_sessions WHERE user_id = $1`, userID).Scan(&id)
	if postgres.IsNoRows(err) {
		return "", nil
	}
	return id, err
}

// SaveSessionID remembers the ACP session id.
func (r *Repository) SaveSessionID(ctx context.Context, userID uuid.UUID, sessionID string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO agent_sessions (user_id, session_id, updated_at) VALUES ($1,$2,now())
		ON CONFLICT (user_id) DO UPDATE SET session_id = EXCLUDED.session_id, updated_at = now()`, userID, sessionID)
	return err
}

// ForgetSession removes the stored session id (a new session must be created).
func (r *Repository) ForgetSession(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM agent_sessions WHERE user_id = $1`, userID)
	return err
}

// Persona is the agent's name and tone plus the user's language.
type Persona struct {
	Name     string
	Tone     domain.AgentTone
	Language string
}

// Persona loads the user's agent persona.
func (r *Repository) Persona(ctx context.Context, userID uuid.UUID) (Persona, error) {
	var p Persona
	err := r.pool.QueryRow(ctx, `SELECT agent_name, agent_tone, language FROM users WHERE id = $1`, userID).Scan(&p.Name, &p.Tone, &p.Language)
	return p, err
}

// InTx exposes a transaction for message + attachment linking.
func (r *Repository) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return postgres.InTx(ctx, r.pool, fn)
}
