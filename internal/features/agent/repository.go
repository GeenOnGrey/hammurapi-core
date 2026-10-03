package agent

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Message is a chat message.
type Message struct {
	ID          uuid.UUID       `json:"id"`
	Role        string          `json:"role"` // user | agent
	Mode        string          `json:"mode"` // general | spec
	Context     *MessageContext `json:"context"`
	Content     string          `json:"content"`
	IsVoice     bool            `json:"isVoice"`
	Attachments []AttachmentRef `json:"attachments"`
	// PLT.HMR-0004: the model of an answer, the error class of a failed
	// user message and the message a retry repeats.
	Model      *string    `json:"model"`
	ErrorClass *string    `json:"errorClass"`
	RetryOf    *uuid.UUID `json:"retryOf"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// MessageContext is the context a message was written in.
type MessageContext struct {
	Type string       `json:"type"`
	Key  string       `json:"key"`
	Area *domain.Area `json:"area"`
}

// AttachmentRef is an attachment shown with a message.
type AttachmentRef struct {
	ID       uuid.UUID `json:"id"`
	FileName string    `json:"fileName"`
	MimeType string    `json:"mimeType"`
}

// Repository persists chat messages and the users' Pi sessions.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository creates a repository.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// NewMessage is a message to store.
type NewMessage struct {
	UserID       uuid.UUID
	Role, Mode   string
	Context      *resolved
	Content      string
	IsVoice      bool
	Model        string
	ConnectionID *uuid.UUID
	RetryOf      *uuid.UUID
}

// Insert stores a message.
func (r *Repository) Insert(ctx context.Context, q postgres.Querier, m NewMessage) (uuid.UUID, time.Time, error) {
	var id uuid.UUID
	var at time.Time
	var ctype, ckey *string
	var area *domain.Area
	if m.Context != nil {
		ctype, ckey, area = &m.Context.Type, &m.Context.Key, m.Context.Area
	}
	var model *string
	if m.Model != "" {
		model = &m.Model
	}
	err := q.QueryRow(ctx, `INSERT INTO chat_messages (user_id, role, mode, context_type, context_key, area, content, is_voice, model, connection_id, retry_of)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id, created_at`,
		m.UserID, m.Role, m.Mode, ctype, ckey, area, m.Content, m.IsVoice, model, m.ConnectionID, m.RetryOf).Scan(&id, &at)
	return id, at, err
}

const messageSelect = `SELECT m.id, m.role, m.mode, m.context_type, m.context_key, m.area, m.content, m.is_voice, m.model, m.error_class, m.retry_of, m.created_at
	FROM chat_messages m`

func scanMessage(row pgx.Row) (*Message, error) {
	var m Message
	var ctype, ckey *string
	var area *domain.Area
	if err := row.Scan(&m.ID, &m.Role, &m.Mode, &ctype, &ckey, &area, &m.Content, &m.IsVoice, &m.Model, &m.ErrorClass, &m.RetryOf, &m.CreatedAt); err != nil {
		return nil, err
	}
	if ctype != nil && ckey != nil {
		m.Context = &MessageContext{Type: *ctype, Key: *ckey, Area: area}
	}
	m.Attachments = []AttachmentRef{}
	return &m, nil
}

// Get returns a message of the user.
func (r *Repository) Get(ctx context.Context, userID, id uuid.UUID) (*Message, error) {
	m, err := scanMessage(r.pool.QueryRow(ctx, messageSelect+` WHERE m.id = $1 AND m.user_id = $2`, id, userID))
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("message_not_found", "message not found")
	}
	return m, err
}

// AttachmentIDs lists the attachments of a message in upload order.
func (r *Repository) AttachmentIDs(ctx context.Context, messageID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM attachments WHERE message_id = $1 ORDER BY created_at`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetErrorClass marks a user message whose processing failed.
func (r *Repository) SetErrorClass(ctx context.Context, id uuid.UUID, class string) error {
	_, err := r.pool.Exec(ctx, `UPDATE chat_messages SET error_class = $2 WHERE id = $1`, id, class)
	return err
}

// History lists messages newest first.
func (r *Repository) History(ctx context.Context, userID uuid.UUID, page httpx.Page) ([]Message, error) {
	args := []any{userID, page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` AND (m.created_at, m.id::text) < ($3, $4)`
	}
	rows, err := r.pool.Query(ctx, messageSelect+` WHERE m.user_id = $1`+cond+` ORDER BY m.created_at DESC, m.id::text DESC LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	var out []Message
	ids := []uuid.UUID{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *m)
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

// ─── Pi sessions of the chat (pi_sessions) ──────────────────────────

// SessionRow is the user's active chat session.
type SessionRow struct {
	ID          uuid.UUID
	OperatorID  string
	SnapshotKey string
	Model       string
}

// OpenSession returns the user's active chat session row, creating it.
func (r *Repository) OpenSession(ctx context.Context, userID uuid.UUID) (*SessionRow, error) {
	var s SessionRow
	err := r.pool.QueryRow(ctx, `INSERT INTO pi_sessions (user_id, scenario, model) VALUES ($1, 'chat', '')
		ON CONFLICT (user_id) WHERE closed_at IS NULL AND user_id IS NOT NULL DO UPDATE SET last_active_at = now()
		RETURNING id, COALESCE(operator_id,''), COALESCE(snapshot_key,''), model`, userID).Scan(&s.ID, &s.OperatorID, &s.SnapshotKey, &s.Model)
	return &s, err
}

// SetSessionOperator records the operator session of a row.
func (r *Repository) SetSessionOperator(ctx context.Context, id uuid.UUID, operatorID, model, thinking string, conn uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE pi_sessions SET operator_id = $2, model = $3, thinking = $4, connection_id = $5, last_active_at = now() WHERE id = $1`,
		id, operatorID, model, thinking, nullID(conn))
	return err
}

// SetSessionModel records a model change of a running session.
func (r *Repository) SetSessionModel(ctx context.Context, id uuid.UUID, model, thinking string, conn uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE pi_sessions SET model = $2, thinking = $3, connection_id = $4 WHERE id = $1`, id, model, thinking, nullID(conn))
	return err
}

func nullID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// SetSnapshot records the saved Pi session file.
func (r *Repository) SetSnapshot(ctx context.Context, id uuid.UUID, key string) error {
	_, err := r.pool.Exec(ctx, `UPDATE pi_sessions SET snapshot_key = $2 WHERE id = $1`, id, key)
	return err
}

// ClearOperator forgets the operator session (closed or lost).
func (r *Repository) ClearOperator(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE pi_sessions SET operator_id = NULL WHERE id = $1`, id)
	return err
}

// TouchSession records activity.
func (r *Repository) TouchSession(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE pi_sessions SET last_active_at = now() WHERE user_id = $1 AND closed_at IS NULL`, userID)
	return err
}

// CloseSession ends the user's chat session; the next one starts from the history.
func (r *Repository) CloseSession(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE pi_sessions SET closed_at = now(), operator_id = NULL WHERE user_id = $1 AND closed_at IS NULL`, userID)
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
