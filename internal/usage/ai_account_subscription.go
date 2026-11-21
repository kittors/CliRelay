package usage

import (
	"database/sql"
	"strings"
	"time"
)

// Where an account's subscription dates came from.
const (
	// SubscriptionSourceProbe is a live read of the provider's billing endpoint.
	SubscriptionSourceProbe = "probe"
	// SubscriptionSourceSignedClaims is the snapshot a provider wrote into an
	// OAuth token. Codex freezes it at login: refreshed tokens keep the claims,
	// so it only describes the billing cycle that was current back then.
	SubscriptionSourceSignedClaims = "signed_claims"
	// SubscriptionSourceMigration came over from the legacy per-tenant table,
	// which was itself filled from token claims.
	SubscriptionSourceMigration = "migration"
)

// Subscription states, set by a live billing read only.
const (
	SubscriptionStateActive    = "active"    // renews automatically
	SubscriptionStateCanceling = "canceling" // renewal turned off; paid until it ends
	SubscriptionStatePastDue   = "past_due"  // renewal charge failed; in the grace period
	SubscriptionStateInactive  = "inactive"  // no paid subscription
)

// AIAccountSubscription is what is known about one account's paid plan cycle.
// The fields form one set: a write replaces all of them, so dates read from one
// source are never mixed with dates from another.
type AIAccountSubscription struct {
	StartedAt *time.Time `json:"subscription_started_at,omitempty"`
	ExpiresAt *time.Time `json:"subscription_expires_at,omitempty"`
	Source    string     `json:"subscription_source,omitempty"`
	// RenewsAt is the next automatic charge; empty once renewal is turned off.
	RenewsAt *time.Time `json:"subscription_renews_at,omitempty"`
	// CancelsAt is set when the owner turned renewal off: the plan ends then.
	CancelsAt *time.Time `json:"subscription_cancels_at,omitempty"`
	Period    string     `json:"subscription_period,omitempty"`
	State     string     `json:"subscription_state,omitempty"`
	// CheckedAt is when the provider last confirmed this set (live reads only).
	CheckedAt *time.Time `json:"subscription_checked_at,omitempty"`
}

// Presentable drops a token snapshot whose end has passed. Such a snapshot says
// nothing about the plan today: the claims are never refreshed after login, so
// a plan that renewed every month would read as expired weeks ago. A live read
// is kept even when past its end, since that is what the provider last said.
func (s AIAccountSubscription) Presentable(now time.Time) AIAccountSubscription {
	if s.Source == SubscriptionSourceProbe || s.Source == "" {
		return s
	}
	if s.ExpiresAt == nil || !s.ExpiresAt.After(now) {
		return AIAccountSubscription{}
	}
	return s
}

// subscriptionColumns are the ai_account_subject_status columns that hold an
// AIAccountSubscription, in the order of args and scan targets below.
var subscriptionColumns = []string{
	"subscription_started_at", "subscription_expires_at", "subscription_source",
	"subscription_renews_at", "subscription_cancels_at", "subscription_period",
	"subscription_state", "subscription_checked_at",
}

var subscriptionColumnList = strings.Join(subscriptionColumns, ", ")

// subscriptionUpsertAssignments keeps the stored set unless the incoming one
// may replace it. An empty source carries nothing, and only another live read
// may replace a live read: token claims and migrated values are older by
// construction, so letting them win would undo the probe every quota round.
var subscriptionUpsertAssignments = func() string {
	keep := "excluded.subscription_source = '' OR (excluded.subscription_source <> 'probe' AND ai_account_subject_status.subscription_source = 'probe')"
	parts := make([]string, len(subscriptionColumns))
	for i, column := range subscriptionColumns {
		parts[i] = column + " = CASE WHEN " + keep + " THEN ai_account_subject_status." + column + " ELSE excluded." + column + " END"
	}
	return strings.Join(parts, ",\n\t\t\t")
}()

func (s AIAccountSubscription) args() []any {
	return []any{
		nullableTimeValue(s.StartedAt), nullableTimeValue(s.ExpiresAt), strings.TrimSpace(s.Source),
		nullableTimeValue(s.RenewsAt), nullableTimeValue(s.CancelsAt), strings.TrimSpace(s.Period),
		strings.TrimSpace(s.State), nullableTimeValue(s.CheckedAt),
	}
}

// subscriptionScan receives the subscriptionColumns of one row.
type subscriptionScan struct {
	started, expires, renews, cancels, checked sql.NullString
	source, period, state                      string
}

func (s *subscriptionScan) targets() []any {
	return []any{&s.started, &s.expires, &s.source, &s.renews, &s.cancels, &s.period, &s.state, &s.checked}
}

func (s *subscriptionScan) value() AIAccountSubscription {
	return AIAccountSubscription{
		StartedAt: parseNullableTime(s.started), ExpiresAt: parseNullableTime(s.expires), Source: s.source,
		RenewsAt: parseNullableTime(s.renews), CancelsAt: parseNullableTime(s.cancels), Period: s.period,
		State: s.state, CheckedAt: parseNullableTime(s.checked),
	}
}

// applySQLiteSharedSubjectAdditiveColumns adds the columns introduced after the
// shared tables were first created. ADD COLUMN fails once a column exists,
// which is the expected outcome on every later start.
func applySQLiteSharedSubjectAdditiveColumns(db *sql.DB) {
	// Additive migration for SQLite databases created before cycle token projection.
	_, _ = db.Exec(`ALTER TABLE ai_account_subject_usage_buckets ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`)
	// Additive migration for databases created before quota freshness tracking.
	_, _ = db.Exec(`ALTER TABLE ai_account_subject_status ADD COLUMN quota_observed_at DATETIME`)
	backfillSQLiteQuotaObservedAt(db)
	// Billing-cycle details read live from the provider; postgres adds them in
	// aiAccountSubscriptionCycleSQL.
	for _, ddl := range []string{
		`ALTER TABLE ai_account_subject_status ADD COLUMN subscription_renews_at DATETIME`,
		`ALTER TABLE ai_account_subject_status ADD COLUMN subscription_cancels_at DATETIME`,
		`ALTER TABLE ai_account_subject_status ADD COLUMN subscription_period TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ai_account_subject_status ADD COLUMN subscription_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE ai_account_subject_status ADD COLUMN subscription_checked_at DATETIME`,
	} {
		_, _ = db.Exec(ddl)
	}
}
