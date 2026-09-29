package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
)

// UserRow is a users row.
type UserRow struct {
	ID          uuid.UUID
	ProviderUID string
	Username    string
	DisplayName string
	AvatarURL   *string
	Language    string
	Theme       string
	AgentName   string
	AgentTone   domain.AgentTone
	GlobalAdmin bool
	CreatedAt   time.Time
}

// EncryptedToken is a user_git_tokens row.
type EncryptedToken struct {
	Access    []byte
	Refresh   []byte
	ExpiresAt time.Time
}

// Repository persists users, sessions and tokens.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository creates a repository.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// UpsertUser inserts or refreshes a user by provider id. created is true on first login.
func (r *Repository) UpsertUser(ctx context.Context, u UserRow) (row UserRow, created bool, err error) {
	err = r.pool.QueryRow(ctx, `
		INSERT INTO users (provider_uid, username, display_name, avatar_url, language, agent_name, agent_tone, is_global_admin)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (provider_uid) DO UPDATE SET username = EXCLUDED.username,
			display_name = EXCLUDED.display_name, avatar_url = EXCLUDED.avatar_url
		RETURNING id, provider_uid, username, display_name, avatar_url, language, theme, agent_name, agent_tone,
			is_global_admin, created_at, (xmax = 0)`,
		u.ProviderUID, u.Username, u.DisplayName, u.AvatarURL, u.Language, u.AgentName, u.AgentTone, u.GlobalAdmin).
		Scan(&row.ID, &row.ProviderUID, &row.Username, &row.DisplayName, &row.AvatarURL, &row.Language, &row.Theme,
			&row.AgentName, &row.AgentTone, &row.GlobalAdmin, &row.CreatedAt, &created)
	return row, created, err
}

// SaveToken stores encrypted tokens.
func (r *Repository) SaveToken(ctx context.Context, userID uuid.UUID, t EncryptedToken) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO user_git_tokens (user_id, access_token_enc, refresh_token_enc, expires_at, updated_at)
		VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (user_id) DO UPDATE SET access_token_enc = EXCLUDED.access_token_enc,
			refresh_token_enc = EXCLUDED.refresh_token_enc, expires_at = EXCLUDED.expires_at, updated_at = now()`,
		userID, t.Access, t.Refresh, t.ExpiresAt)
	return err
}

// Token loads encrypted tokens.
func (r *Repository) Token(ctx context.Context, userID uuid.UUID) (*EncryptedToken, error) {
	var t EncryptedToken
	err := r.pool.QueryRow(ctx, `SELECT access_token_enc, refresh_token_enc, expires_at FROM user_git_tokens WHERE user_id = $1`, userID).
		Scan(&t.Access, &t.Refresh, &t.ExpiresAt)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return &t, err
}

// DeleteToken removes tokens (forces re-login).
func (r *Repository) DeleteToken(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM user_git_tokens WHERE user_id = $1`, userID)
	return err
}

// CreateSession creates a browser session.
func (r *Repository) CreateSession(ctx context.Context, userID uuid.UUID, csrf string, ttl time.Duration) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `INSERT INTO user_sessions (user_id, csrf_token, expires_at) VALUES ($1,$2,$3) RETURNING id`,
		userID, csrf, time.Now().Add(ttl)).Scan(&id)
	return id, err
}

// SessionRow is a live session with its user.
type SessionRow struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	CSRFToken  string
	LastSeenAt time.Time
}

// Session loads a live session.
func (r *Repository) Session(ctx context.Context, id uuid.UUID) (*SessionRow, error) {
	var s SessionRow
	err := r.pool.QueryRow(ctx, `SELECT id, user_id, csrf_token, last_seen_at FROM user_sessions WHERE id = $1 AND expires_at > now()`, id).
		Scan(&s.ID, &s.UserID, &s.CSRFToken, &s.LastSeenAt)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return &s, err
}

// TouchSession extends a session.
func (r *Repository) TouchSession(ctx context.Context, id uuid.UUID, ttl time.Duration) error {
	_, err := r.pool.Exec(ctx, `UPDATE user_sessions SET last_seen_at = now(), expires_at = $2 WHERE id = $1`, id, time.Now().Add(ttl))
	return err
}

// DeleteSession removes a session.
func (r *Repository) DeleteSession(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM user_sessions WHERE id = $1`, id)
	return err
}

// Principal loads the user with roles. Roles are read on every request, so a
// role change takes effect with the next request.
func (r *Repository) Principal(ctx context.Context, userID uuid.UUID) (*domain.Principal, error) {
	var username, name string
	var ga bool
	if err := r.pool.QueryRow(ctx, `SELECT username, display_name, is_global_admin FROM users WHERE id = $1`, userID).
		Scan(&username, &name, &ga); err != nil {
		if postgres.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	p := domain.NewPrincipal(userID, username, name, ga)
	rows, err := r.pool.Query(ctx, `
		SELECT 'admin', area::text, '' FROM area_admins WHERE user_id = $1
		UNION ALL
		SELECT 'expert', e.kind::text, d.key FROM domain_experts e JOIN domains d ON d.id = e.domain_id WHERE e.user_id = $1
		UNION ALL
		SELECT 'owner', '', s.key FROM service_owners o JOIN services s ON s.id = o.service_id WHERE o.user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, val, key string
		if err := rows.Scan(&kind, &val, &key); err != nil {
			return nil, err
		}
		switch kind {
		case "admin":
			p.GrantAreaAdmin(domain.Area(val))
		case "expert":
			p.GrantExpert(key, domain.ExpertKind(val))
		case "owner":
			p.GrantOwner(key)
		}
	}
	return p, rows.Err()
}

// User loads a user row.
func (r *Repository) User(ctx context.Context, id uuid.UUID) (*UserRow, error) {
	var u UserRow
	err := r.pool.QueryRow(ctx, `SELECT id, provider_uid, username, display_name, avatar_url, language, theme, agent_name,
		agent_tone, is_global_admin, created_at FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.ProviderUID, &u.Username, &u.DisplayName, &u.AvatarURL, &u.Language, &u.Theme, &u.AgentName,
			&u.AgentTone, &u.GlobalAdmin, &u.CreatedAt)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return &u, err
}
