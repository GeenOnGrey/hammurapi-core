// Package attachments stores chat files in S3 with metadata in Postgres.
package attachments

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/storage"
)

// Attachment is attachment metadata.
type Attachment struct {
	ID        uuid.UUID  `json:"id"`
	FileName  string     `json:"fileName"`
	MimeType  string     `json:"mimeType"`
	SizeBytes int64      `json:"sizeBytes"`
	MessageID *uuid.UUID `json:"messageId"`
	Feature   *string    `json:"feature"`
	Mode      *string    `json:"mode"`
	CreatedAt time.Time  `json:"createdAt"`
	S3Key     string     `json:"-"`
	UserID    uuid.UUID  `json:"-"`
}

// Service implements attachment use cases.
type Service struct {
	pool         *pgxpool.Pool
	store        storage.Storage
	maxBytes     int64
	allowedTypes map[string]bool
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, store storage.Storage, maxBytes int64, allowed []string) *Service {
	m := map[string]bool{}
	for _, t := range allowed {
		m[strings.ToLower(t)] = true
	}
	return &Service{pool: pool, store: store, maxBytes: maxBytes, allowedTypes: m}
}

// DetectAllowed sniffs the content type and checks it against the allow-list,
// walking up the detected type's parents (e.g. docx → zip is not accepted,
// but docx itself is).
func DetectAllowed(data []byte, allowed map[string]bool) (string, bool) {
	for m := mimetype.Detect(data); m != nil; m = m.Parent() {
		base := strings.ToLower(strings.TrimSpace(strings.Split(m.String(), ";")[0]))
		if allowed[base] {
			return base, true
		}
		for t := range allowed {
			if m.Is(t) { // Is also matches the type's aliases
				return t, true
			}
		}
		// Only the leaf type counts for containers (zip, ole): stop at generic parents.
		if base == "application/zip" || base == "application/x-ole-storage" || base == "application/octet-stream" {
			break
		}
	}
	return mimetype.Detect(data).String(), false
}

// Upload stores a file for the user.
func (s *Service) Upload(ctx context.Context, userID uuid.UUID, name string, r io.Reader) (*Attachment, error) {
	data, err := io.ReadAll(io.LimitReader(r, s.maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxBytes {
		return nil, apperr.TooLarge("file_too_large", fmt.Sprintf("file exceeds %d bytes", s.maxBytes)).With("maxBytes", s.maxBytes)
	}
	mime, ok := DetectAllowed(data, s.allowedTypes)
	if !ok {
		return nil, apperr.Unsupported("unsupported_type", "file type is not allowed: "+mime)
	}
	id := uuid.New()
	key := fmt.Sprintf("attachments/%s/%s", userID, id)
	if err := s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), mime); err != nil {
		return nil, err
	}
	name = sanitizeName(name)
	a := &Attachment{ID: id, FileName: name, MimeType: mime, SizeBytes: int64(len(data)), S3Key: key, UserID: userID}
	if err := s.pool.QueryRow(ctx, `INSERT INTO attachments (id, user_id, file_name, mime_type, size_bytes, s3_key)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`, id, userID, name, mime, a.SizeBytes, key).Scan(&a.CreatedAt); err != nil {
		_ = s.store.Delete(context.WithoutCancel(ctx), key)
		return nil, err
	}
	return a, nil
}

func sanitizeName(n string) string {
	n = path.Base(strings.ReplaceAll(n, "\\", "/"))
	if n == "." || n == "/" || n == "" {
		return "file"
	}
	if r := []rune(n); len(r) > 200 {
		n = string(r[:200])
	}
	return n
}

const selectCols = `SELECT a.id, a.file_name, a.mime_type, a.size_bytes, a.message_id, m.context_key, m.mode::text, a.created_at, a.s3_key, a.user_id
	FROM attachments a LEFT JOIN chat_messages m ON m.id = a.message_id`

func scan(row interface{ Scan(...any) error }) (*Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.FileName, &a.MimeType, &a.SizeBytes, &a.MessageID, &a.Feature, &a.Mode, &a.CreatedAt, &a.S3Key, &a.UserID)
	return &a, err
}

// List returns the user's files, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, page httpx.Page) (httpx.List[Attachment], error) {
	args := []any{userID, page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` AND (a.created_at, a.id::text) < ($3, $4)`
	}
	rows, err := s.pool.Query(ctx, selectCols+` WHERE a.user_id = $1`+cond+` ORDER BY a.created_at DESC, a.id::text DESC LIMIT $2`, args...)
	if err != nil {
		return httpx.List[Attachment]{}, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return httpx.List[Attachment]{}, err
		}
		out = append(out, *a)
	}
	return httpx.NewList(out, page.Limit, func(a Attachment) (time.Time, string) { return a.CreatedAt, a.ID.String() }), rows.Err()
}

// Get loads an attachment owned by the user.
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (*Attachment, error) {
	a, err := scan(s.pool.QueryRow(ctx, selectCols+` WHERE a.id = $1`, id))
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("attachment_not_found", "attachment not found")
	}
	if err != nil {
		return nil, err
	}
	if a.UserID != userID {
		return nil, apperr.Forbidden("forbidden", "not your attachment")
	}
	return a, nil
}

// Read returns the content of an owned attachment.
func (s *Service) Read(ctx context.Context, userID, id uuid.UUID) (*Attachment, []byte, error) {
	a, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}
	rc, err := s.store.Get(ctx, a.S3Key)
	if err != nil {
		return nil, nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	return a, data, err
}

// Link attaches uploaded files to a chat message (owner-checked).
func (s *Service) Link(ctx context.Context, q postgres.Querier, userID, messageID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	tag, err := q.Exec(ctx, `UPDATE attachments SET message_id = $1 WHERE id = ANY($2) AND user_id = $3`, messageID, ids, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != int64(len(ids)) {
		return apperr.Forbidden("forbidden", "unknown or foreign attachment")
	}
	return nil
}

// Routes mounts /attachments.
func (s *Service) Routes(r chi.Router) {
	r.Post("/attachments", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBytes+1<<20)
		file, hdr, err := r.FormFile("file")
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return apperr.TooLarge("file_too_large", "file is too large").With("maxBytes", s.maxBytes)
			}
			return apperr.BadRequest("invalid_upload", "multipart field 'file' is required")
		}
		defer file.Close()
		a, err := s.Upload(r.Context(), p.UserID, hdr.Filename, file)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, a)
		return nil
	}))
	r.Get("/attachments", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		page, err := httpx.ParsePage(r)
		if err != nil {
			return err
		}
		l, err := s.List(r.Context(), p.UserID, page)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, l)
		return nil
	}))
	r.Get("/attachments/{id}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return err
		}
		a, data, err := s.Read(r.Context(), p.UserID, id)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", a.MimeType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEscape(a.FileName)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
		return nil
	}))
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
