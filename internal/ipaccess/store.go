package ipaccess

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
)

// ErrRuleNotFound is returned when an id does not resolve to a rule.
var ErrRuleNotFound = errors.New("ip access: rule not found")

// ErrDuplicateRule is returned when the same effect already covers this CIDR.
var ErrDuplicateRule = errors.New("ip access: a rule with this effect and CIDR already exists")

// Store persists allow/deny rules.
type Store struct {
	db *sql.DB
}

// NewStore wires a store to the runtime database. A nil db yields a store whose
// methods are no-ops returning errors, which keeps callers free of nil checks in
// deployments that run without Postgres.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Available reports whether persistence is usable.
func (s *Store) Available() bool { return s != nil && s.db != nil }

const ruleColumns = `id,cidr,family,effect,source,reason,note,enabled,expires_at,created_by,created_at,updated_at,hit_count,last_hit_at`

func scanRule(scanner interface{ Scan(...any) error }) (Rule, error) {
	var (
		rule      Rule
		expires   sql.NullTime
		createdBy sql.NullString
		lastHit   sql.NullTime
		effect    string
		source    string
	)
	if err := scanner.Scan(&rule.ID, &rule.CIDR, &rule.Family, &effect, &source, &rule.Reason,
		&rule.Note, &rule.Enabled, &expires, &createdBy, &rule.CreatedAt, &rule.UpdatedAt,
		&rule.HitCount, &lastHit); err != nil {
		return Rule{}, err
	}
	rule.Effect = Effect(effect)
	rule.Source = Source(source)
	if expires.Valid {
		t := expires.Time
		rule.ExpiresAt = &t
	}
	if lastHit.Valid {
		t := lastHit.Time
		rule.LastHitAt = &t
	}
	if createdBy.Valid {
		rule.CreatedBy = createdBy.String
	}
	return rule, nil
}

// LoadActive returns every rule that could match right now.
//
// Expired rules are filtered in SQL as well as in the matcher: without the SQL
// filter a long-abandoned deployment would rebuild a snapshot from thousands of
// dead auto-ban rows on every refresh.
func (s *Store) LoadActive(ctx context.Context, now time.Time) ([]Rule, error) {
	if !s.Available() {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleColumns+` FROM ip_access_rules
		WHERE enabled = true AND (expires_at IS NULL OR expires_at > ?)`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rules []Rule
	for rows.Next() {
		rule, scanErr := scanRule(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

// ListFilter narrows a rule listing.
type ListFilter struct {
	Effect  string
	Source  string
	Enabled *bool
	Search  string
	Page    int
	Size    int
}

const (
	defaultListSize = 50
	maxListSize     = 200
)

func (f ListFilter) normalized() ListFilter {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Size < 1 {
		f.Size = defaultListSize
	}
	if f.Size > maxListSize {
		f.Size = maxListSize
	}
	return f
}

// List returns a page of rules together with the total row count.
func (s *Store) List(ctx context.Context, filter ListFilter) ([]Rule, int, error) {
	if !s.Available() {
		return nil, 0, nil
	}
	filter = filter.normalized()
	where := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if effect := strings.TrimSpace(filter.Effect); effect != "" {
		where = append(where, "effect = ?")
		args = append(args, effect)
	}
	if source := strings.TrimSpace(filter.Source); source != "" {
		where = append(where, "source = ?")
		args = append(args, source)
	}
	if filter.Enabled != nil {
		where = append(where, "enabled = ?")
		args = append(args, *filter.Enabled)
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		where = append(where, "(cidr ILIKE ? OR note ILIKE ? OR reason ILIKE ?)")
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern, pattern)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ip_access_rules`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// Deny rules first, then newest: an operator opening this page is far more
	// often chasing an active block than reviewing the allow list.
	query := `SELECT ` + ruleColumns + ` FROM ip_access_rules` + clause +
		` ORDER BY effect DESC, created_at DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Size, (filter.Page-1)*filter.Size)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	rules := make([]Rule, 0, filter.Size)
	for rows.Next() {
		rule, scanErr := scanRule(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		rules = append(rules, rule)
	}
	return rules, total, rows.Err()
}

// Get resolves one rule by id.
func (s *Store) Get(ctx context.Context, id string) (Rule, error) {
	if !s.Available() {
		return Rule{}, ErrRuleNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM ip_access_rules WHERE id = ?`, id)
	rule, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, ErrRuleNotFound
	}
	return rule, err
}

// CreateInput is an operator-supplied rule.
type CreateInput struct {
	CIDR      string
	Effect    Effect
	Source    Source
	Reason    string
	Note      string
	ExpiresAt *time.Time
	CreatedBy string
}

// Create stores a new rule, normalising the CIDR first so the uniqueness
// constraint sees canonical values.
func (s *Store) Create(ctx context.Context, in CreateInput) (Rule, error) {
	if !s.Available() {
		return Rule{}, errors.New("ip access: storage unavailable")
	}
	cidr, family, err := NormalizeCIDR(in.CIDR)
	if err != nil {
		return Rule{}, err
	}
	if in.Effect != EffectAllow && in.Effect != EffectDeny {
		return Rule{}, ErrInvalidEffect
	}
	source := in.Source
	if source != SourceAuto {
		source = SourceManual
	}
	var createdBy any
	if strings.TrimSpace(in.CreatedBy) != "" {
		createdBy = in.CreatedBy
	}
	var expires any
	if in.ExpiresAt != nil {
		expires = in.ExpiresAt.UTC()
	}
	id := uuid.NewString()
	_, err = configsync.Exec(ctx, s.db, ruleChangeEvents, `INSERT INTO ip_access_rules
		(id,cidr,family,effect,source,reason,note,enabled,expires_at,created_by)
		VALUES (?,?,?,?,?,?,?,true,?,?)`,
		id, cidr, family, string(in.Effect), string(source), in.Reason, in.Note, expires, createdBy)
	if err != nil {
		if isUniqueViolation(err) {
			return Rule{}, ErrDuplicateRule
		}
		return Rule{}, err
	}
	return s.Get(ctx, id)
}

// UpdateInput carries the mutable fields. Nil means "leave unchanged", so a
// partial update from the panel cannot blank out fields it never rendered.
type UpdateInput struct {
	Enabled      *bool
	Note         *string
	ExpiresAt    **time.Time
	ClearExpires bool
}

// Update mutates an existing rule.
func (s *Store) Update(ctx context.Context, id string, in UpdateInput) (Rule, error) {
	if !s.Available() {
		return Rule{}, ErrRuleNotFound
	}
	sets := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, *in.Enabled)
	}
	if in.Note != nil {
		sets = append(sets, "note = ?")
		args = append(args, *in.Note)
	}
	if in.ClearExpires {
		sets = append(sets, "expires_at = NULL")
	} else if in.ExpiresAt != nil && *in.ExpiresAt != nil {
		sets = append(sets, "expires_at = ?")
		args = append(args, (*in.ExpiresAt).UTC())
	}
	if len(sets) == 0 {
		return s.Get(ctx, id)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id)
	result, err := configsync.Exec(ctx, s.db, ruleChangeEvents,
		`UPDATE ip_access_rules SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return Rule{}, err
	}
	if affected, affErr := result.RowsAffected(); affErr == nil && affected == 0 {
		return Rule{}, ErrRuleNotFound
	}
	return s.Get(ctx, id)
}

// Delete removes a rule.
func (s *Store) Delete(ctx context.Context, id string) error {
	if !s.Available() {
		return ErrRuleNotFound
	}
	result, err := configsync.Exec(ctx, s.db, ruleChangeEvents, `DELETE FROM ip_access_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, affErr := result.RowsAffected(); affErr == nil && affected == 0 {
		return ErrRuleNotFound
	}
	return nil
}

// UpsertAutoBan records an automatic ban.
//
// An existing manual rule for the same CIDR is left untouched: an operator's
// decision outranks the engine's, in both directions. An existing automatic ban
// has its expiry extended rather than duplicated, which is also what makes the
// escalation ladder observable — repeat_count grows on the same row.
func (s *Store) UpsertAutoBan(ctx context.Context, cidr, reason string, expiresAt time.Time) (Rule, bool, error) {
	if !s.Available() {
		return Rule{}, false, errors.New("ip access: storage unavailable")
	}
	normalized, family, err := NormalizeCIDR(cidr)
	if err != nil {
		return Rule{}, false, err
	}
	existing, err := s.findByEffectCIDR(ctx, EffectDeny, normalized)
	if err != nil && !errors.Is(err, ErrRuleNotFound) {
		return Rule{}, false, err
	}
	if err == nil {
		if existing.Source == SourceManual {
			return existing, false, nil
		}
		if _, updateErr := configsync.Exec(ctx, s.db, ruleChangeEvents,
			`UPDATE ip_access_rules SET expires_at = ?, enabled = true, reason = ?, updated_at = now() WHERE id = ?`,
			expiresAt.UTC(), reason, existing.ID); updateErr != nil {
			return Rule{}, false, updateErr
		}
		refreshed, getErr := s.Get(ctx, existing.ID)
		return refreshed, true, getErr
	}

	id := uuid.NewString()
	if _, err = configsync.Exec(ctx, s.db, ruleChangeEvents, `INSERT INTO ip_access_rules
		(id,cidr,family,effect,source,reason,note,enabled,expires_at)
		VALUES (?,?,?,'deny','auto',?,'',true,?)`,
		id, normalized, family, reason, expiresAt.UTC()); err != nil {
		if isUniqueViolation(err) {
			// Another slot won the race; its row is equally valid.
			rule, getErr := s.findByEffectCIDR(ctx, EffectDeny, normalized)
			return rule, false, getErr
		}
		return Rule{}, false, err
	}
	rule, err := s.Get(ctx, id)
	return rule, true, err
}

func (s *Store) findByEffectCIDR(ctx context.Context, effect Effect, cidr string) (Rule, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+ruleColumns+` FROM ip_access_rules WHERE effect = ? AND cidr = ?`, string(effect), cidr)
	rule, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, ErrRuleNotFound
	}
	return rule, err
}

// RecordHits folds buffered match counts back into the rules.
//
// Counts are batched by the caller instead of incremented per request: a deny
// rule exists precisely because something is hammering the server, so a write
// per match would turn a block into a database load generator.
func (s *Store) RecordHits(ctx context.Context, hits map[string]int64, at time.Time) error {
	if !s.Available() || len(hits) == 0 {
		return nil
	}
	for id, count := range hits {
		if count <= 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ip_access_rules SET hit_count = hit_count + ?, last_hit_at = ? WHERE id = ?`,
			count, at.UTC(), id); err != nil {
			return err
		}
	}
	return nil
}

// PurgeExpiredAuto deletes automatic rules that lapsed before cutoff. Manual
// rules are never swept, expired or not: an operator may keep a lapsed entry as
// a record of what was banned and why.
func (s *Store) PurgeExpiredAuto(ctx context.Context, cutoff time.Time) (int64, error) {
	if !s.Available() {
		return 0, nil
	}
	result, err := configsync.Exec(ctx, s.db, ruleChangeEvents,
		`DELETE FROM ip_access_rules WHERE source = 'auto' AND expires_at IS NOT NULL AND expires_at < ?`,
		cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// isUniqueViolation recognises the duplicate-key error without importing a
// driver-specific package: the compat layer may surface either the pq or pgx
// error type, and both spell the SQLSTATE into the message.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "23505") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint")
}

// ruleChangeEvents announce a rule write to the other cluster nodes. They
// refresh their rule snapshot on it instead of waiting for the periodic poll,
// so a ban placed on one node applies on every node within about a second.
var ruleChangeEvents = []cluster.ConfigEvent{configsync.Event(configsync.DomainIPAccessRules, "")}

func init() {
	// The protection policy is a runtime_settings row, but its receivers are
	// in this package, so its writes are announced under their own domain.
	configsync.RouteRuntimeSettingKey(SettingKey, configsync.DomainIPAccessPolicy)
}
