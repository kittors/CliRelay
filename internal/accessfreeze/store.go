package accessfreeze

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrUnavailable is returned when no runtime database is configured.
	ErrUnavailable = errors.New("access freezes: runtime database unavailable")
	// ErrNotFound is a freeze id the tenant does not have.
	ErrNotFound = errors.New("access freeze not found")
	// ErrAlreadyReleased is a release of a freeze that no longer holds.
	ErrAlreadyReleased = errors.New("access freeze already released")
)

// DefaultThreshold is the number of refusals that freezes a requester in a
// tenant that has not set its own: the first one.
const DefaultThreshold = 1

// MaxThreshold bounds the threshold an administrator may set.
const MaxThreshold = 100

// Store keeps strikes, freezes and thresholds in the runtime database
// (PostgreSQL; see internal/storage/postgres/access_freeze_migrations.go).
type Store struct {
	db *sql.DB
}

// NewStore returns a store over db; a nil db makes every call ErrUnavailable.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) usable() bool { return s != nil && s.db != nil }

const freezeColumns = `id, tenant_id::text, subject_kind, subject_id, provider, reason_code, upstream_message,
  model, auth_label, request_id, strike_count, frozen_at, released_at, released_by, release_note`

const strikeColumns = `id, tenant_id::text, subject_kind, subject_id, provider, reason_code, upstream_message,
  model, auth_label, request_id, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFreeze(row rowScanner) (Freeze, error) {
	var (
		f        Freeze
		kind     string
		released sql.NullTime
	)
	if err := row.Scan(&f.ID, &f.Subject.TenantID, &kind, &f.Subject.ID, &f.Provider, &f.ReasonCode, &f.UpstreamMessage,
		&f.Model, &f.AuthLabel, &f.RequestID, &f.StrikeCount, &f.FrozenAt, &released, &f.ReleasedBy, &f.ReleaseNote); err != nil {
		return Freeze{}, err
	}
	f.Subject.Kind = SubjectKind(kind)
	if released.Valid {
		at := released.Time
		f.ReleasedAt = &at
	}
	return f, nil
}

func scanStrike(row rowScanner) (Strike, error) {
	var (
		st   Strike
		kind string
	)
	if err := row.Scan(&st.ID, &st.Subject.TenantID, &kind, &st.Subject.ID, &st.Provider, &st.ReasonCode, &st.UpstreamMessage,
		&st.Model, &st.AuthLabel, &st.RequestID, &st.CreatedAt); err != nil {
		return Strike{}, err
	}
	st.Subject.Kind = SubjectKind(kind)
	return st, nil
}

// RecordStrike stores one refusal and returns how many refusals the subject
// has had on the provider since its last freeze there was released, this one
// included: what the threshold is compared with.
func (s *Store) RecordStrike(ctx context.Context, st Strike) (int, error) {
	if !s.usable() {
		return 0, ErrUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO access_freeze_strikes
		  (tenant_id, subject_kind, subject_id, provider, reason_code, upstream_message, model, auth_label, request_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		st.Subject.TenantID, string(st.Subject.Kind), st.Subject.ID, normalizeProvider(st.Provider), st.ReasonCode,
		capUpstreamMessage(st.UpstreamMessage), st.Model, st.AuthLabel, st.RequestID, st.CreatedAt); err != nil {
		return 0, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `
		SELECT count(*) FROM access_freeze_strikes
		WHERE tenant_id = $1 AND subject_kind = $2 AND subject_id = $3 AND provider = $4
		  AND created_at > COALESCE((
		    SELECT max(released_at) FROM access_freezes
		    WHERE tenant_id = $1 AND subject_kind = $2 AND subject_id = $3 AND provider = $4
		  ), '-infinity'::timestamptz)`,
		st.Subject.TenantID, string(st.Subject.Kind), st.Subject.ID, normalizeProvider(st.Provider)).Scan(&count); err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

// CountOnStanding adds a refusal to the subject's standing freeze of the
// provider, if one stands, and returns it.
func (s *Store) CountOnStanding(ctx context.Context, subject Subject, provider string) (Freeze, bool, error) {
	if !s.usable() {
		return Freeze{}, false, ErrUnavailable
	}
	f, err := scanFreeze(s.db.QueryRowContext(ctx, `
		UPDATE access_freezes SET strike_count = strike_count + 1
		WHERE tenant_id = $1 AND subject_kind = $2 AND subject_id = $3 AND provider = $4 AND released_at IS NULL
		RETURNING `+freezeColumns,
		subject.TenantID, string(subject.Kind), subject.ID, normalizeProvider(provider)))
	if errors.Is(err, sql.ErrNoRows) {
		return Freeze{}, false, nil
	}
	if err != nil {
		return Freeze{}, false, err
	}
	return f, true, nil
}

// Freeze creates the subject's freeze of the strike's provider, resting on
// strikeCount refusals. When a freeze already stands (a refusal of another
// request in flight got there first) it returns that one, unchanged, and false.
func (s *Store) Freeze(ctx context.Context, st Strike, strikeCount int, at time.Time) (Freeze, bool, error) {
	if !s.usable() {
		return Freeze{}, false, ErrUnavailable
	}
	provider := normalizeProvider(st.Provider)
	f, err := scanFreeze(s.db.QueryRowContext(ctx, `
		INSERT INTO access_freezes
		  (tenant_id, subject_kind, subject_id, provider, reason_code, upstream_message, model, auth_label, request_id, strike_count, frozen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, subject_kind, subject_id, provider) WHERE released_at IS NULL DO NOTHING
		RETURNING `+freezeColumns,
		st.Subject.TenantID, string(st.Subject.Kind), st.Subject.ID, provider, st.ReasonCode,
		capUpstreamMessage(st.UpstreamMessage), st.Model, st.AuthLabel, st.RequestID, strikeCount, at))
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Freeze{}, false, err
	}
	f, err = scanFreeze(s.db.QueryRowContext(ctx, `
		SELECT `+freezeColumns+` FROM access_freezes
		WHERE tenant_id = $1 AND subject_kind = $2 AND subject_id = $3 AND provider = $4 AND released_at IS NULL`,
		st.Subject.TenantID, string(st.Subject.Kind), st.Subject.ID, provider))
	if err != nil {
		return Freeze{}, false, err
	}
	return f, false, nil
}

// ListActive returns every standing freeze of every tenant: what the
// request path is held to.
func (s *Store) ListActive(ctx context.Context) ([]Freeze, error) {
	if !s.usable() {
		return nil, ErrUnavailable
	}
	return s.queryFreezes(ctx, `SELECT `+freezeColumns+` FROM access_freezes WHERE released_at IS NULL ORDER BY id`)
}

// Filter narrows List. Empty fields do not filter.
type Filter struct {
	// Status is "active", "released", or "" for both.
	Status      string
	SubjectKind SubjectKind
	SubjectID   string
	Provider    string
	Limit       int
}

// List returns a tenant's freezes, newest first.
func (s *Store) List(ctx context.Context, tenantID string, filter Filter) ([]Freeze, error) {
	if !s.usable() {
		return nil, ErrUnavailable
	}
	query := `SELECT ` + freezeColumns + ` FROM access_freezes WHERE tenant_id = $1`
	args := []any{tenantID}
	switch filter.Status {
	case "active":
		query += ` AND released_at IS NULL`
	case "released":
		query += ` AND released_at IS NOT NULL`
	}
	if filter.SubjectKind != "" {
		args = append(args, string(filter.SubjectKind))
		query += ` AND subject_kind = $` + strconv.Itoa(len(args))
	}
	if id := strings.TrimSpace(filter.SubjectID); id != "" {
		args = append(args, id)
		query += ` AND subject_id = $` + strconv.Itoa(len(args))
	}
	if provider := normalizeProvider(filter.Provider); provider != "" {
		args = append(args, provider)
		query += ` AND provider = $` + strconv.Itoa(len(args))
	}
	args = append(args, clampLimit(filter.Limit))
	query += ` ORDER BY frozen_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))
	return s.queryFreezes(ctx, query, args...)
}

// ListStrikes returns a subject's refusals, newest first; an empty provider
// lists them all.
func (s *Store) ListStrikes(ctx context.Context, subject Subject, provider string, limit int) ([]Strike, error) {
	if !s.usable() {
		return nil, ErrUnavailable
	}
	query := `SELECT ` + strikeColumns + ` FROM access_freeze_strikes
		WHERE tenant_id = $1 AND subject_kind = $2 AND subject_id = $3`
	args := []any{subject.TenantID, string(subject.Kind), subject.ID}
	if provider = normalizeProvider(provider); provider != "" {
		args = append(args, provider)
		query += ` AND provider = $` + strconv.Itoa(len(args))
	}
	args = append(args, clampLimit(limit))
	query += ` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Strike, 0)
	for rows.Next() {
		st, err := scanStrike(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Release lifts one of the tenant's freezes.
func (s *Store) Release(ctx context.Context, tenantID string, id int64, by, note string, at time.Time) (Freeze, error) {
	if !s.usable() {
		return Freeze{}, ErrUnavailable
	}
	f, err := scanFreeze(s.db.QueryRowContext(ctx, `
		UPDATE access_freezes SET released_at = $3, released_by = $4, release_note = $5
		WHERE tenant_id = $1 AND id = $2 AND released_at IS NULL
		RETURNING `+freezeColumns,
		tenantID, id, at, by, note))
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Freeze{}, err
	}
	var exists bool
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM access_freezes WHERE tenant_id = $1 AND id = $2)`, tenantID, id).Scan(&exists); err != nil {
		return Freeze{}, err
	}
	if exists {
		return Freeze{}, ErrAlreadyReleased
	}
	return Freeze{}, ErrNotFound
}

// Threshold is the tenant's threshold: DefaultThreshold until one is set, and
// 0 when the tenant only records refusals without freezing.
func (s *Store) Threshold(ctx context.Context, tenantID string) (int, error) {
	if !s.usable() {
		return 0, ErrUnavailable
	}
	var threshold int
	err := s.db.QueryRowContext(ctx, `SELECT threshold FROM access_freeze_settings WHERE tenant_id = $1`, tenantID).Scan(&threshold)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultThreshold, nil
	}
	return threshold, err
}

// SetThreshold stores the tenant's threshold.
func (s *Store) SetThreshold(ctx context.Context, tenantID string, threshold int, by string, at time.Time) error {
	if !s.usable() {
		return ErrUnavailable
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO access_freeze_settings (tenant_id, threshold, updated_at, updated_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET threshold = EXCLUDED.threshold, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		tenantID, threshold, at, by)
	return err
}

func (s *Store) queryFreezes(ctx context.Context, query string, args ...any) ([]Freeze, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Freeze, 0)
	for rows.Next() {
		f, err := scanFreeze(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

const (
	defaultListLimit = 200
	maxListLimit     = 1000
)

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}
