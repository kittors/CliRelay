package billing

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

const planSelectColumns = `
	id, tenant_id, name, description, kind, price_cents, duration_days,
	quota_amount_usd, quota_requests, quota_tokens, rate_usd_per_cny,
	min_amount_cents, max_amount_cents, permission_profile_id, badge,
	sort_order, enabled, created_at, updated_at`

func scanPlan(row interface{ Scan(dest ...any) error }) (Plan, error) {
	var p Plan
	var kind string
	var rawCreated, rawUpdated any
	if err := row.Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Description, &kind, &p.PriceCents, &p.DurationDays,
		&p.QuotaAmountUSD, &p.QuotaRequests, &p.QuotaTokens, &p.RateUSDPerCNY,
		&p.MinAmountCents, &p.MaxAmountCents, &p.PermissionProfileID, &p.Badge,
		&p.SortOrder, &p.Enabled, &rawCreated, &rawUpdated,
	); err != nil {
		return Plan{}, err
	}
	p.Kind = PlanKind(kind)
	if ts, ok := parseAnyTime(rawCreated); ok {
		p.CreatedAt = ts
	}
	if ts, ok := parseAnyTime(rawUpdated); ok {
		p.UpdatedAt = ts
	}
	return p, nil
}

func validatePlanInput(input PlanInput) (PlanInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.PermissionProfileID = strings.TrimSpace(input.PermissionProfileID)
	input.Badge = strings.TrimSpace(input.Badge)
	if input.Name == "" || len([]rune(input.Name)) > 80 {
		return input, fieldErr("name", "name is required and must be at most 80 characters")
	}
	if len([]rune(input.Description)) > 300 {
		return input, fieldErr("description", "description must be at most 300 characters")
	}
	if len([]rune(input.Badge)) > 24 {
		return input, fieldErr("badge", "badge must be at most 24 characters")
	}
	if input.PriceCents < 0 || input.PriceCents > 100000000 {
		return input, fieldErr("price_cents", "price must be between 0 and 1,000,000 CNY")
	}
	if input.QuotaAmountUSD < 0 || math.IsNaN(input.QuotaAmountUSD) || math.IsInf(input.QuotaAmountUSD, 0) {
		return input, fieldErr("quota_amount_usd", "quota_amount_usd must be non-negative")
	}
	input.QuotaAmountUSD = math.Round(input.QuotaAmountUSD*100) / 100
	if input.QuotaRequests < 0 {
		return input, fieldErr("quota_requests", "quota_requests must be non-negative")
	}
	if input.QuotaTokens < 0 {
		return input, fieldErr("quota_tokens", "quota_tokens must be non-negative")
	}
	switch input.Kind {
	case PlanKindSubscription:
		if input.PriceCents <= 0 {
			return input, fieldErr("price_cents", "subscription price must be greater than 0")
		}
		if input.DurationDays <= 0 || input.DurationDays > 3650 {
			return input, fieldErr("duration_days", "duration_days must be between 1 and 3650")
		}
		input.RateUSDPerCNY = 0
	case PlanKindQuotaPack:
		if input.PriceCents <= 0 {
			return input, fieldErr("price_cents", "quota pack price must be greater than 0")
		}
		if input.QuotaAmountUSD <= 0 && input.QuotaRequests <= 0 && input.QuotaTokens <= 0 {
			return input, fieldErr("quota_amount_usd", "quota pack must grant USD, requests, or tokens")
		}
		input.DurationDays = 0
		input.RateUSDPerCNY = 0
	case PlanKindCustomQuota:
		if input.RateUSDPerCNY <= 0 || math.IsNaN(input.RateUSDPerCNY) || math.IsInf(input.RateUSDPerCNY, 0) {
			return input, fieldErr("rate_usd_per_cny", "rate_usd_per_cny must be greater than 0")
		}
		input.RateUSDPerCNY = math.Round(input.RateUSDPerCNY*10000) / 10000
		if input.MinAmountCents <= 0 {
			input.MinAmountCents = 100
		}
		if input.MaxAmountCents < input.MinAmountCents {
			return input, fieldErr("max_amount_cents", "max_amount_cents must be >= min_amount_cents")
		}
		if input.PriceCents < input.MinAmountCents || input.PriceCents > input.MaxAmountCents {
			input.PriceCents = input.MinAmountCents
		}
		input.DurationDays = 0
		input.QuotaAmountUSD = 0
		input.QuotaRequests = 0
		input.QuotaTokens = 0
		input.PermissionProfileID = ""
	case PlanKindDailyReset:
		if input.PriceCents <= 0 {
			return input, fieldErr("price_cents", "daily reset price must be greater than 0")
		}
		input.DurationDays = 0
		input.QuotaAmountUSD = 0
		input.QuotaRequests = 0
		input.QuotaTokens = 0
		input.RateUSDPerCNY = 0
		input.PermissionProfileID = ""
	default:
		return input, fieldErr("kind", "unsupported plan kind")
	}
	return input, nil
}

func (s *Store) ListPlans(ctx context.Context, tenantID string, enabledOnly bool) ([]Plan, error) {
	if !s.usable() {
		return nil, ErrUnavailable
	}
	query := `SELECT ` + planSelectColumns + ` FROM billing_plans WHERE tenant_id = ?`
	if enabledOnly {
		query += ` AND enabled = true`
	}
	query += ` ORDER BY sort_order ASC, created_at ASC`
	rows, err := s.db.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Plan, 0)
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPlan(ctx context.Context, tenantID, planID string) (Plan, error) {
	if !s.usable() {
		return Plan{}, ErrUnavailable
	}
	p, err := scanPlan(s.db.QueryRowContext(ctx,
		`SELECT `+planSelectColumns+` FROM billing_plans WHERE tenant_id = ? AND id = ?`,
		tenantID, strings.TrimSpace(planID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	return p, err
}

func (s *Store) CreatePlan(ctx context.Context, tenantID string, input PlanInput) (Plan, error) {
	if !s.usable() {
		return Plan{}, ErrUnavailable
	}
	clean, err := validatePlanInput(input)
	if err != nil {
		return Plan{}, err
	}
	enabled := true
	if clean.Enabled != nil {
		enabled = *clean.Enabled
	}
	now := time.Now().UTC()
	p := Plan{
		ID:                  uuid.NewString(),
		TenantID:            tenantID,
		Name:                clean.Name,
		Description:         clean.Description,
		Kind:                clean.Kind,
		PriceCents:          clean.PriceCents,
		DurationDays:        clean.DurationDays,
		QuotaAmountUSD:      clean.QuotaAmountUSD,
		QuotaRequests:       clean.QuotaRequests,
		QuotaTokens:         clean.QuotaTokens,
		RateUSDPerCNY:       clean.RateUSDPerCNY,
		MinAmountCents:      clean.MinAmountCents,
		MaxAmountCents:      clean.MaxAmountCents,
		PermissionProfileID: clean.PermissionProfileID,
		Badge:               clean.Badge,
		SortOrder:           clean.SortOrder,
		Enabled:             enabled,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_plans (
			id, tenant_id, name, description, kind, price_cents, duration_days,
			quota_amount_usd, quota_requests, quota_tokens, rate_usd_per_cny,
			min_amount_cents, max_amount_cents, permission_profile_id, badge,
			sort_order, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		p.ID, p.TenantID, p.Name, p.Description, string(p.Kind), p.PriceCents, p.DurationDays,
		p.QuotaAmountUSD, p.QuotaRequests, p.QuotaTokens, p.RateUSDPerCNY,
		p.MinAmountCents, p.MaxAmountCents, p.PermissionProfileID, p.Badge,
		p.SortOrder, p.Enabled, now, now,
	); err != nil {
		return Plan{}, err
	}
	return p, nil
}

func (s *Store) UpdatePlan(ctx context.Context, tenantID, planID string, input PlanInput) (Plan, error) {
	if !s.usable() {
		return Plan{}, ErrUnavailable
	}
	existing, err := s.GetPlan(ctx, tenantID, planID)
	if err != nil {
		return Plan{}, err
	}
	clean, err := validatePlanInput(input)
	if err != nil {
		return Plan{}, err
	}
	enabled := existing.Enabled
	if clean.Enabled != nil {
		enabled = *clean.Enabled
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE billing_plans
		   SET name = ?, description = ?, kind = ?, price_cents = ?, duration_days = ?,
		       quota_amount_usd = ?, quota_requests = ?, quota_tokens = ?, rate_usd_per_cny = ?,
		       min_amount_cents = ?, max_amount_cents = ?, permission_profile_id = ?, badge = ?,
		       sort_order = ?, enabled = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ?
	`,
		clean.Name, clean.Description, string(clean.Kind), clean.PriceCents, clean.DurationDays,
		clean.QuotaAmountUSD, clean.QuotaRequests, clean.QuotaTokens, clean.RateUSDPerCNY,
		clean.MinAmountCents, clean.MaxAmountCents, clean.PermissionProfileID, clean.Badge,
		clean.SortOrder, enabled, now, tenantID, existing.ID,
	); err != nil {
		return Plan{}, err
	}
	return s.GetPlan(ctx, tenantID, existing.ID)
}

func (s *Store) DeletePlan(ctx context.Context, tenantID, planID string) error {
	if !s.usable() {
		return ErrUnavailable
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM billing_plans WHERE tenant_id = ? AND id = ?`, tenantID, strings.TrimSpace(planID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
