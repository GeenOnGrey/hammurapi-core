// Package profile implements the user profile: language, theme, my domains,
// agent name and tone.
package profile

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Profile is the user's settings.
type Profile struct {
	Language  string   `json:"language"`
	Theme     string   `json:"theme"`
	Domains   []string `json:"domains"`
	AgentName string   `json:"agentName"`
	AgentTone string   `json:"agentTone"`
}

// Patch is a partial update.
type Patch struct {
	Language  *string   `json:"language"`
	Theme     *string   `json:"theme"`
	Domains   *[]string `json:"domains"`
	AgentName *string   `json:"agentName"`
	AgentTone *string   `json:"agentTone"`
}

// Service implements profile use cases.
type Service struct {
	pool *pgxpool.Pool
	// OnAgentChanged is called when name or tone changes, so the next agent
	// session starts with the new personality.
	OnAgentChanged func(userID uuid.UUID)
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Get loads the profile.
func (s *Service) Get(ctx context.Context, userID uuid.UUID) (*Profile, error) {
	var p Profile
	if err := s.pool.QueryRow(ctx, `SELECT language, theme, agent_name, agent_tone FROM users WHERE id = $1`, userID).
		Scan(&p.Language, &p.Theme, &p.AgentName, &p.AgentTone); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT d.key FROM user_domains ud JOIN domains d ON d.id = ud.domain_id WHERE ud.user_id = $1 ORDER BY d.key`, userID)
	if err != nil {
		return nil, err
	}
	p.Domains, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if p.Domains == nil {
		p.Domains = []string{}
	}
	return &p, err
}

// Update applies a patch.
func (s *Service) Update(ctx context.Context, userID uuid.UUID, in Patch) (*Profile, error) {
	if in.Language != nil && !domain.ValidLanguage(*in.Language) {
		return nil, apperr.Unprocessable("invalid_language", "unsupported language")
	}
	if in.Theme != nil && *in.Theme != "light" && *in.Theme != "dark" {
		return nil, apperr.Unprocessable("invalid_theme", "theme must be light or dark")
	}
	if in.AgentTone != nil && !domain.AgentTone(*in.AgentTone).Valid() {
		return nil, apperr.Unprocessable("invalid_tone", "unknown agent tone")
	}
	if in.AgentName != nil {
		n := strings.TrimSpace(*in.AgentName)
		if n == "" || len([]rune(n)) > 40 {
			return nil, apperr.Unprocessable("invalid_agent_name", "agent name must be 1-40 characters")
		}
		in.AgentName = &n
	}
	agentChanged := false
	err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var oldName, oldTone string
		if err := tx.QueryRow(ctx, `SELECT agent_name, agent_tone FROM users WHERE id = $1`, userID).Scan(&oldName, &oldTone); err != nil {
			return err
		}
		agentChanged = (in.AgentName != nil && *in.AgentName != oldName) || (in.AgentTone != nil && *in.AgentTone != oldTone)
		if _, err := tx.Exec(ctx, `UPDATE users SET language = COALESCE($2, language), theme = COALESCE($3, theme),
			agent_name = COALESCE($4, agent_name), agent_tone = COALESCE($5::agent_tone, agent_tone) WHERE id = $1`,
			userID, in.Language, in.Theme, in.AgentName, in.AgentTone); err != nil {
			return err
		}
		if in.Domains != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM user_domains WHERE user_id = $1`, userID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO user_domains (user_id, domain_id) SELECT $1, id FROM domains WHERE key = ANY($2)`,
				userID, *in.Domains); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if agentChanged && s.OnAgentChanged != nil {
		s.OnAgentChanged(userID)
	}
	return s.Get(ctx, userID)
}

// Routes mounts profile routes.
func (s *Service) Routes(r chi.Router) {
	r.Get("/profile", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		pr, err := s.Get(r.Context(), p.UserID)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, pr)
		return nil
	}))
	r.Patch("/profile", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in Patch
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		pr, err := s.Update(r.Context(), p.UserID, in)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, pr)
		return nil
	}))
	r.Get("/agent-tones", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, 200, domain.Tones)
	})
}
