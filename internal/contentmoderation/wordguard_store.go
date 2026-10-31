package contentmoderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// The word policy tables. The statements run on SQLite and PostgreSQL alike, so
// booleans are integers and times are text, as in the profile tables. A policy
// is one row with its rules as a JSON array: saving replaces the whole list
// atomically, and the rules are only ever read as a list.
//
// The migration adds two tables and nothing else; no existing table or row is
// touched, so an old build keeps running on the upgraded database.
const wordPolicyTablesSQL = `
CREATE TABLE IF NOT EXISTS content_moderation_word_policies (
  tenant_id TEXT NOT NULL,
  scope_type TEXT NOT NULL,
  scope_id TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  action TEXT NOT NULL DEFAULT '',
  replacement TEXT NOT NULL DEFAULT '',
  scan_scope TEXT NOT NULL DEFAULT '',
  reply_filter INTEGER NOT NULL DEFAULT -1,
  fold_traditional_chinese INTEGER NOT NULL DEFAULT -1,
  inherit_tenant INTEGER NOT NULL DEFAULT 1,
  block_message TEXT NOT NULL DEFAULT '',
  rule_count INTEGER NOT NULL DEFAULT 0,
  rules_json TEXT NOT NULL DEFAULT '[]',
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, scope_type, scope_id)
);
`

// platformWordTenantID is the tenant the platform policy is stored under.
func platformWordTenantID() string { return coreauth.NormalizedTenantID("") }

// WordPolicySummary is a policy without its rules, for listings.
type WordPolicySummary struct {
	ScopeType string    `json:"scope_type"`
	ScopeID   string    `json:"scope_id,omitempty"`
	Enabled   bool      `json:"enabled"`
	Action    string    `json:"action"`
	RuleCount int       `json:"rule_count"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

const wordPolicyColumns = `tenant_id,scope_type,scope_id,enabled,action,replacement,scan_scope,reply_filter,fold_traditional_chinese,inherit_tenant,block_message,rules_json,version,created_at,updated_at`

func scanWordPolicy(scanner interface{ Scan(...any) error }) (WordPolicy, error) {
	var p WordPolicy
	var enabled, reply, fold, inherit int
	var rulesJSON, createdAt, updatedAt string
	if err := scanner.Scan(&p.TenantID, &p.ScopeType, &p.ScopeID, &enabled, &p.Action, &p.Replacement, &p.ScanScope, &reply, &fold, &inherit,
		&p.BlockMessage, &rulesJSON, &p.Version, &createdAt, &updatedAt); err != nil {
		return WordPolicy{}, err
	}
	p.Enabled, p.InheritTenant = enabled != 0, inherit != 0
	p.ReplyFilter, p.FoldTraditionalChinese = triFromColumn(reply), triFromColumn(fold)
	p.Rules = []WordRule{}
	if err := json.Unmarshal([]byte(rulesJSON), &p.Rules); err != nil {
		return WordPolicy{}, fmt.Errorf("decode word rules: %w", err)
	}
	p.CreatedAt, p.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return p, nil
}

// triColumn stores an optional boolean: -1 for "inherit", otherwise 0 or 1.
func triColumn(value *bool) int {
	if value == nil {
		return -1
	}
	return boolColumn(*value)
}

func triFromColumn(value int) *bool {
	if value < 0 {
		return nil
	}
	set := value != 0
	return &set
}

// GetWordPolicy returns one policy, or ErrWordPolicyNotFound.
func (s *Store) GetWordPolicy(ctx context.Context, tenantID, scopeType, scopeID string) (WordPolicy, error) {
	if s == nil || s.db == nil {
		return WordPolicy{}, ErrUnavailable
	}
	p, err := scanWordPolicy(s.db.QueryRowContext(ctx, `SELECT `+wordPolicyColumns+` FROM content_moderation_word_policies WHERE tenant_id=$1 AND scope_type=$2 AND scope_id=$3`,
		strings.TrimSpace(tenantID), scopeType, strings.TrimSpace(scopeID)))
	if errors.Is(err, sql.ErrNoRows) {
		return WordPolicy{}, ErrWordPolicyNotFound
	}
	return p, err
}

// PutWordPolicy creates or replaces a policy and returns it as stored.
// expectedVersion, when given, must match the stored version (zero meaning
// "there is none yet"); without it the save is unconditional, which is what a
// client that never read the policy first expects. The policy must already have
// passed Normalize.
func (s *Store) PutWordPolicy(ctx context.Context, p WordPolicy, expectedVersion *int64) (WordPolicy, error) {
	if s == nil || s.db == nil {
		return WordPolicy{}, ErrUnavailable
	}
	rules, err := json.Marshal(nonNilRules(p.Rules))
	if err != nil {
		return WordPolicy{}, err
	}
	now := time.Now().UTC()
	p.TenantID = strings.TrimSpace(p.TenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WordPolicy{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current int64
	var createdAt string
	err = tx.QueryRowContext(ctx, `SELECT version, created_at FROM content_moderation_word_policies WHERE tenant_id=$1 AND scope_type=$2 AND scope_id=$3`,
		p.TenantID, p.ScopeType, p.ScopeID).Scan(&current, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if expectedVersion != nil && *expectedVersion != 0 {
			return WordPolicy{}, ErrWordPolicyVersionConflict
		}
		p.Version, p.CreatedAt, p.UpdatedAt = 1, now, now
		_, err = tx.ExecContext(ctx, `INSERT INTO content_moderation_word_policies
			(tenant_id,scope_type,scope_id,enabled,action,replacement,scan_scope,reply_filter,fold_traditional_chinese,inherit_tenant,block_message,rule_count,rules_json,version,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			p.TenantID, p.ScopeType, p.ScopeID, boolColumn(p.Enabled), p.Action, p.Replacement, p.ScanScope, triColumn(p.ReplyFilter), triColumn(p.FoldTraditionalChinese),
			boolColumn(p.InheritTenant), p.BlockMessage, len(p.Rules), string(rules), p.Version, formatTime(p.CreatedAt), formatTime(p.UpdatedAt))
		if isUniqueViolation(err) {
			// Another save created it between our read and write.
			return WordPolicy{}, ErrWordPolicyVersionConflict
		}
		if err != nil {
			return WordPolicy{}, err
		}
	case err != nil:
		return WordPolicy{}, err
	default:
		if expectedVersion != nil && *expectedVersion != current {
			return WordPolicy{}, ErrWordPolicyVersionConflict
		}
		p.Version, p.CreatedAt, p.UpdatedAt = current+1, parseTime(createdAt), now
		result, execErr := tx.ExecContext(ctx, `UPDATE content_moderation_word_policies SET
			enabled=$1,action=$2,replacement=$3,scan_scope=$4,reply_filter=$5,fold_traditional_chinese=$6,inherit_tenant=$7,block_message=$8,
			rule_count=$9,rules_json=$10,version=$11,updated_at=$12
			WHERE tenant_id=$13 AND scope_type=$14 AND scope_id=$15 AND version=$16`,
			boolColumn(p.Enabled), p.Action, p.Replacement, p.ScanScope, triColumn(p.ReplyFilter), triColumn(p.FoldTraditionalChinese), boolColumn(p.InheritTenant),
			p.BlockMessage, len(p.Rules), string(rules), p.Version, formatTime(p.UpdatedAt),
			p.TenantID, p.ScopeType, p.ScopeID, current)
		if execErr != nil {
			return WordPolicy{}, execErr
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
			return WordPolicy{}, rowsErr
		} else if rows == 0 {
			return WordPolicy{}, ErrWordPolicyVersionConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return WordPolicy{}, err
	}
	return p, nil
}

func nonNilRules(rules []WordRule) []WordRule {
	if rules == nil {
		return []WordRule{}
	}
	return rules
}

// DeleteWordPolicy removes a policy, or reports ErrWordPolicyNotFound.
func (s *Store) DeleteWordPolicy(ctx context.Context, tenantID, scopeType, scopeID string) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM content_moderation_word_policies WHERE tenant_id=$1 AND scope_type=$2 AND scope_id=$3`,
		strings.TrimSpace(tenantID), scopeType, strings.TrimSpace(scopeID))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrWordPolicyNotFound
	}
	return nil
}

// ListWordKeyPolicies lists the API key policies of a tenant, without rules.
func (s *Store) ListWordKeyPolicies(ctx context.Context, tenantID string) ([]WordPolicySummary, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope_id,enabled,action,rule_count,version,updated_at FROM content_moderation_word_policies
		WHERE tenant_id=$1 AND scope_type=$2 ORDER BY scope_id`, strings.TrimSpace(tenantID), WordScopeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WordPolicySummary, 0)
	for rows.Next() {
		summary := WordPolicySummary{ScopeType: WordScopeKey}
		var enabled int
		var updatedAt string
		if err := rows.Scan(&summary.ScopeID, &enabled, &summary.Action, &summary.RuleCount, &summary.Version, &updatedAt); err != nil {
			return nil, err
		}
		summary.Enabled = enabled != 0
		summary.UpdatedAt = parseTime(updatedAt)
		out = append(out, summary)
	}
	return out, rows.Err()
}

// wordPolicyKey identifies a policy in the cache.
type wordPolicyKey struct{ tenantID, scopeType, scopeID string }

// maxCachedWordPolicies bounds the policy cache. Keys are the bulk of the
// entries; when the bound is reached the cache starts over.
const maxCachedWordPolicies = 8192

// wordPolicyCache keeps decoded policies, so that a request costs one small
// query (the versions of the policies that apply) and decodes a policy's rules
// only when it has changed. The version is bumped by every save, on whichever
// node made it, so nodes need no other way to learn of a change.
type wordPolicyCache struct {
	mu    sync.Mutex
	items map[wordPolicyKey]*WordPolicy
}

func (c *wordPolicyCache) get(key wordPolicyKey) *WordPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items[key]
}

func (c *wordPolicyCache) put(key wordPolicyKey, policy *WordPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil || len(c.items) >= maxCachedWordPolicies {
		c.items = make(map[wordPolicyKey]*WordPolicy)
	}
	c.items[key] = policy
}

// WordPolicies are the policies that apply to one request. Any may be nil.
type WordPolicies struct {
	Platform, Tenant, Key *WordPolicy
}

// LoadWordPolicies returns the policies governing a request of tenantID made
// with the API key keyID (empty when unknown). The result is shared and must
// not be modified.
//
// If the database cannot be read, the policies last seen are returned together
// with the error, so a database outage does not switch the guard off for
// requests it already knew how to judge.
func (s *Store) LoadWordPolicies(ctx context.Context, tenantID, keyID string) (WordPolicies, error) {
	if s == nil || s.db == nil {
		return WordPolicies{}, ErrUnavailable
	}
	tenantID, keyID = strings.TrimSpace(tenantID), strings.TrimSpace(keyID)
	platformKey := wordPolicyKey{platformWordTenantID(), WordScopePlatform, ""}
	tenantKey := wordPolicyKey{tenantID, WordScopeTenant, ""}
	keyKey := wordPolicyKey{tenantID, WordScopeKey, keyID}
	remembered := WordPolicies{Platform: s.wordPolicies.get(platformKey), Tenant: s.wordPolicies.get(tenantKey)}
	if keyID != "" {
		remembered.Key = s.wordPolicies.get(keyKey)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id,scope_type,scope_id,version,updated_at FROM content_moderation_word_policies
		WHERE (tenant_id=$1 AND scope_type=$2 AND scope_id='')
		   OR (tenant_id=$3 AND scope_type=$4 AND scope_id='')
		   OR (tenant_id=$3 AND scope_type=$5 AND scope_id=$6)`,
		platformKey.tenantID, WordScopePlatform, tenantID, WordScopeTenant, WordScopeKey, keyID)
	if err != nil {
		return remembered, err
	}
	// A policy that is deleted and created again starts over at version 1, so the
	// version alone cannot say whether a cached copy is still the stored one: the
	// time of the last save must match as well.
	type header struct {
		key       wordPolicyKey
		version   int64
		updatedAt time.Time
	}
	var headers []header
	for rows.Next() {
		var h header
		var updatedAt string
		if err := rows.Scan(&h.key.tenantID, &h.key.scopeType, &h.key.scopeID, &h.version, &updatedAt); err != nil {
			_ = rows.Close()
			return remembered, err
		}
		h.updatedAt = parseTime(updatedAt)
		headers = append(headers, h)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return remembered, err
	}
	_ = rows.Close()
	var found WordPolicies
	for _, h := range headers {
		policy := s.wordPolicies.get(h.key)
		if policy == nil || policy.Version != h.version || !policy.UpdatedAt.Equal(h.updatedAt) {
			loaded, err := s.GetWordPolicy(ctx, h.key.tenantID, h.key.scopeType, h.key.scopeID)
			if errors.Is(err, ErrWordPolicyNotFound) {
				continue // deleted since the listing
			}
			if err != nil {
				return remembered, err
			}
			policy = &loaded
			s.wordPolicies.put(h.key, policy)
		}
		switch h.key.scopeType {
		case WordScopePlatform:
			found.Platform = policy
		case WordScopeTenant:
			found.Tenant = policy
		case WordScopeKey:
			if keyID != "" {
				found.Key = policy
			}
		}
	}
	return found, nil
}
