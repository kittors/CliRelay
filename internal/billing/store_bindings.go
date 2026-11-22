package billing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const bindingSelectColumns = `
	id, tenant_id, target_kind, target_id, end_user_id, enabled,
	plan_ids_json, price_overrides_json, public_code, allow_public_pay,
	note, created_at, updated_at`

func scanBinding(row interface{ Scan(dest ...any) error }) (Binding, error) {
	var b Binding
	var kind, planIDsJSON, overridesJSON string
	var rawCreated, rawUpdated any
	if err := row.Scan(
		&b.ID, &b.TenantID, &kind, &b.TargetID, &b.EndUserID, &b.Enabled,
		&planIDsJSON, &overridesJSON, &b.PublicCode, &b.AllowPublicPay,
		&b.Note, &rawCreated, &rawUpdated,
	); err != nil {
		return Binding{}, err
	}
	b.TargetKind = TargetKind(kind)
	if err := json.Unmarshal([]byte(planIDsJSON), &b.PlanIDs); err != nil || b.PlanIDs == nil {
		b.PlanIDs = []string{}
	}
	if err := json.Unmarshal([]byte(overridesJSON), &b.PriceOverrides); err != nil || b.PriceOverrides == nil {
		b.PriceOverrides = map[string]int{}
	}
	if ts, ok := parseAnyTime(rawCreated); ok {
		b.CreatedAt = ts
	}
	if ts, ok := parseAnyTime(rawUpdated); ok {
		b.UpdatedAt = ts
	}
	return b, nil
}

func generatePublicCode() (string, error) {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Store) ListBindings(ctx context.Context, tenantID string) ([]Binding, error) {
	if !s.usable() {
		return nil, ErrUnavailable
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+bindingSelectColumns+` FROM billing_bindings WHERE tenant_id = ? ORDER BY updated_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Binding, 0)
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) ListBindingsForEndUser(ctx context.Context, tenantID, endUserID string) ([]Binding, error) {
	if !s.usable() {
		return nil, ErrUnavailable
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+bindingSelectColumns+`
		  FROM billing_bindings
		 WHERE tenant_id = ? AND end_user_id = ? AND enabled = true
		 ORDER BY target_kind ASC, created_at ASC
	`, tenantID, strings.TrimSpace(endUserID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Binding, 0)
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetBinding(ctx context.Context, tenantID, bindingID string) (Binding, error) {
	if !s.usable() {
		return Binding{}, ErrUnavailable
	}
	b, err := scanBinding(s.db.QueryRowContext(ctx,
		`SELECT `+bindingSelectColumns+` FROM billing_bindings WHERE tenant_id = ? AND id = ?`,
		tenantID, strings.TrimSpace(bindingID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}

func (s *Store) GetBindingByTarget(ctx context.Context, tenantID string, kind TargetKind, targetID string) (Binding, error) {
	if !s.usable() {
		return Binding{}, ErrUnavailable
	}
	b, err := scanBinding(s.db.QueryRowContext(ctx,
		`SELECT `+bindingSelectColumns+` FROM billing_bindings WHERE tenant_id = ? AND target_kind = ? AND target_id = ?`,
		tenantID, string(kind), strings.TrimSpace(targetID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}

func (s *Store) GetBindingByPublicCode(ctx context.Context, publicCode string) (Binding, error) {
	if !s.usable() {
		return Binding{}, ErrUnavailable
	}
	code := strings.TrimSpace(publicCode)
	if code == "" {
		return Binding{}, ErrNotFound
	}
	b, err := scanBinding(s.db.QueryRowContext(ctx,
		`SELECT `+bindingSelectColumns+` FROM billing_bindings WHERE public_code = ?`,
		code,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}

func (s *Store) UpsertBinding(ctx context.Context, tenantID string, kind TargetKind, targetID, endUserID string, input BindingInput) (Binding, error) {
	if !s.usable() {
		return Binding{}, ErrUnavailable
	}
	existing, err := s.GetBindingByTarget(ctx, tenantID, kind, targetID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Binding{}, err
	}
	id := existing.ID
	if id == "" {
		id = uuid.NewString()
	}
	publicCode := existing.PublicCode
	if input.AllowPublicPay && publicCode == "" {
		publicCode, err = generatePublicCode()
		if err != nil {
			return Binding{}, err
		}
	}
	planIDs := dedupeTrimmed(input.PlanIDs)
	planIDsBytes, err := json.Marshal(planIDs)
	if err != nil {
		return Binding{}, err
	}
	overrides := make(map[string]int, len(input.PriceOverrides))
	for k, v := range input.PriceOverrides {
		k = strings.TrimSpace(k)
		if k != "" && v > 0 {
			overrides[k] = v
		}
	}
	overridesBytes, err := json.Marshal(overrides)
	if err != nil {
		return Binding{}, err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_bindings (
			id, tenant_id, target_kind, target_id, end_user_id, enabled,
			plan_ids_json, price_overrides_json, public_code, allow_public_pay,
			note, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, target_kind, target_id) DO UPDATE SET
			end_user_id = EXCLUDED.end_user_id,
			enabled = EXCLUDED.enabled,
			plan_ids_json = EXCLUDED.plan_ids_json,
			price_overrides_json = EXCLUDED.price_overrides_json,
			public_code = EXCLUDED.public_code,
			allow_public_pay = EXCLUDED.allow_public_pay,
			note = EXCLUDED.note,
			updated_at = EXCLUDED.updated_at
	`,
		id, tenantID, string(kind), strings.TrimSpace(targetID), strings.TrimSpace(endUserID),
		input.Enabled, string(planIDsBytes), string(overridesBytes),
		publicCode, input.AllowPublicPay, strings.TrimSpace(input.Note), now, now,
	); err != nil {
		return Binding{}, err
	}
	return s.GetBindingByTarget(ctx, tenantID, kind, targetID)
}

func (s *Store) RotateBindingPublicCode(ctx context.Context, tenantID, bindingID string) (Binding, error) {
	if !s.usable() {
		return Binding{}, ErrUnavailable
	}
	code, err := generatePublicCode()
	if err != nil {
		return Binding{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE billing_bindings SET public_code = ?, allow_public_pay = true, updated_at = ?
		 WHERE tenant_id = ? AND id = ?
	`, code, time.Now().UTC(), tenantID, strings.TrimSpace(bindingID))
	if err != nil {
		return Binding{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Binding{}, ErrNotFound
	}
	return s.GetBinding(ctx, tenantID, bindingID)
}

func (s *Store) DeleteBinding(ctx context.Context, tenantID, bindingID string) error {
	if !s.usable() {
		return ErrUnavailable
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM billing_bindings WHERE tenant_id = ? AND id = ?`, tenantID, strings.TrimSpace(bindingID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetEntitlement returns the target's subscription expiration timestamp (nil if none).
func (s *Store) GetEntitlement(ctx context.Context, tenantID string, kind TargetKind, targetID string) (*time.Time, error) {
	if !s.usable() {
		return nil, nil
	}
	var raw any
	err := s.db.QueryRowContext(ctx, `
		SELECT expires_at FROM billing_entitlements
		 WHERE tenant_id = ? AND target_kind = ? AND target_id = ?
	`, tenantID, string(kind), strings.TrimSpace(targetID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ts, ok := parseAnyTime(raw); ok {
		return &ts, nil
	}
	return nil, nil
}

// ListEntitlementsForTenant returns maps of end_user_id -> expires_at and api_key_id -> expires_at.
func (s *Store) ListEntitlementsForTenant(ctx context.Context, tenantID string) (map[string]time.Time, map[string]time.Time, error) {
	accounts := make(map[string]time.Time)
	keys := make(map[string]time.Time)
	if !s.usable() {
		return accounts, keys, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_kind, target_id, expires_at
		  FROM billing_entitlements
		 WHERE tenant_id = ? AND expires_at IS NOT NULL
	`, tenantID)
	if err != nil {
		return accounts, keys, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		var raw any
		if err := rows.Scan(&kind, &id, &raw); err != nil {
			return accounts, keys, err
		}
		if ts, ok := parseAnyTime(raw); ok {
			if TargetKind(kind) == TargetKindEndUser {
				accounts[id] = ts
			} else if TargetKind(kind) == TargetKindAPIKey {
				keys[id] = ts
			}
		}
	}
	return accounts, keys, rows.Err()
}

// ListAllExpiries loads all non-null expirations across tenants for the access provider cache.
func (s *Store) ListAllExpiries(ctx context.Context) (map[string]time.Time, map[string]time.Time, error) {
	accounts := make(map[string]time.Time)
	keys := make(map[string]time.Time)
	if !s.usable() {
		return accounts, keys, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_kind, target_id, expires_at
		  FROM billing_entitlements
		 WHERE expires_at IS NOT NULL
	`)
	if err != nil {
		return accounts, keys, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		var raw any
		if err := rows.Scan(&kind, &id, &raw); err != nil {
			return accounts, keys, err
		}
		if ts, ok := parseAnyTime(raw); ok {
			if TargetKind(kind) == TargetKindEndUser {
				accounts[id] = ts
			} else if TargetKind(kind) == TargetKindAPIKey {
				keys[id] = ts
			}
		}
	}
	return accounts, keys, rows.Err()
}

// SetEntitlementExpiry sets or clears (when expiresAt is nil) a target's subscription expiration.
func (s *Store) SetEntitlementExpiry(ctx context.Context, tenantID string, kind TargetKind, targetID string, expiresAt *time.Time) error {
	if !s.usable() {
		return ErrUnavailable
	}
	now := time.Now().UTC()
	if expiresAt == nil {
		_, err := s.db.ExecContext(ctx, `
			DELETE FROM billing_entitlements
			 WHERE tenant_id = ? AND target_kind = ? AND target_id = ?
		`, tenantID, string(kind), strings.TrimSpace(targetID))
		return err
	}
	exp := expiresAt.UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_entitlements (tenant_id, target_kind, target_id, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, target_kind, target_id) DO UPDATE SET
			expires_at = EXCLUDED.expires_at,
			updated_at = EXCLUDED.updated_at
	`, tenantID, string(kind), strings.TrimSpace(targetID), exp, now)
	return err
}

func extendEntitlementExpiryTx(ctx context.Context, tx *sql.Tx, tenantID string, kind TargetKind, targetID string, days int, now time.Time) (time.Time, error) {
	var raw any
	err := tx.QueryRowContext(ctx, `
		SELECT expires_at FROM billing_entitlements
		 WHERE tenant_id = ? AND target_kind = ? AND target_id = ?
	`, tenantID, string(kind), targetID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	base := now.UTC()
	if current, ok := parseAnyTime(raw); ok && current.After(base) {
		base = current
	}
	next := base.Add(time.Duration(days) * 24 * time.Hour)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_entitlements (tenant_id, target_kind, target_id, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, target_kind, target_id) DO UPDATE SET
			expires_at = EXCLUDED.expires_at,
			updated_at = EXCLUDED.updated_at
	`, tenantID, string(kind), targetID, next, now.UTC()); err != nil {
		return time.Time{}, err
	}
	return next, nil
}

func dedupeTrimmed(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
