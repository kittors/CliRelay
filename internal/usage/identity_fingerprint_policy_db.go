package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
)

var (
	ErrIdentityFingerprintPolicyConflict       = errors.New("identity fingerprint policy revision conflict")
	ErrIdentityFingerprintProfileMissing       = errors.New("identity fingerprint profile not found")
	ErrIdentityFingerprintProfileNotSelectable = errors.New("identity fingerprint profile is not safe for outbound selection")
)

func GetIdentityFingerprintAccountPolicy(provider identityfingerprint.Provider, accountKey string) (identityfingerprint.AccountPolicy, error) {
	provider = identityfingerprint.Provider(strings.TrimSpace(string(provider)))
	accountKey = strings.TrimSpace(accountKey)
	fallback := identityfingerprint.NormalizeAccountPolicy(provider, accountKey, identityfingerprint.AccountPolicy{})
	db := getDB()
	if db == nil || provider == "" || accountKey == "" {
		return fallback, nil
	}
	policy, err := getIdentityFingerprintAccountPolicyWith(db, provider, accountKey, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fallback, nil
		}
		return fallback, err
	}
	return policy, nil
}

func SaveIdentityFingerprintAccountPolicy(policy identityfingerprint.AccountPolicy, expectedRevision int64) (identityfingerprint.AccountPolicy, error) {
	policy = identityfingerprint.NormalizeAccountPolicy(policy.Provider, policy.AccountKey, policy)
	if policy.Provider == "" || policy.AccountKey == "" {
		return identityfingerprint.AccountPolicy{}, fmt.Errorf("provider and account_key are required")
	}
	db := getDB()
	if db == nil {
		return identityfingerprint.AccountPolicy{}, fmt.Errorf("identity fingerprint store unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return identityfingerprint.AccountPolicy{}, err
	}
	defer func() { _ = tx.Rollback() }()

	postgres := currentUsageDriver() == "postgres"
	if postgres {
		// Lock a concrete row even for the first write. SELECT FOR UPDATE on a
		// missing row does not serialize concurrent initial policy updates.
		if err := ensureIdentityFingerprintAccountPolicyWith(tx, policy.Provider, policy.AccountKey); err != nil {
			return identityfingerprint.AccountPolicy{}, err
		}
	}
	current, err := getIdentityFingerprintAccountPolicyWith(tx, policy.Provider, policy.AccountKey, postgres)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return identityfingerprint.AccountPolicy{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		current = identityfingerprint.NormalizeAccountPolicy(policy.Provider, policy.AccountKey, identityfingerprint.AccountPolicy{})
		current.Revision = 0
	}
	if current.Revision != expectedRevision {
		return identityfingerprint.AccountPolicy{}, ErrIdentityFingerprintPolicyConflict
	}
	if policy.Strategy == identityfingerprint.AccountStrategyActiveProfile {
		profile, errProfile := getIdentityFingerprintProfileWith(tx, policy.Provider, policy.AccountKey, policy.ActiveProfileKey, postgres)
		if errProfile != nil {
			return identityfingerprint.AccountPolicy{}, errProfile
		}
		if profile == nil {
			return identityfingerprint.AccountPolicy{}, ErrIdentityFingerprintProfileMissing
		}
		if eligible, _ := identityfingerprint.CodexProfileOutboundEligibility(profile); !eligible {
			return identityfingerprint.AccountPolicy{}, ErrIdentityFingerprintProfileNotSelectable
		}
	}
	policy.Revision = current.Revision + 1
	if policy.Revision <= 0 {
		policy.Revision = 1
	}
	policy.UpdatedAt = time.Now().UTC()
	if err := upsertIdentityFingerprintAccountPolicyWith(tx, policy); err != nil {
		return identityfingerprint.AccountPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return identityfingerprint.AccountPolicy{}, err
	}
	notifyIdentityFingerprintInvalidated(policy.Provider, policy.AccountKey)
	return policy, nil
}

func DeleteIdentityFingerprintProfileAndRepairPolicy(provider identityfingerprint.Provider, accountKey, profileKey string) (int64, identityfingerprint.AccountPolicy, error) {
	provider = identityfingerprint.Provider(strings.TrimSpace(string(provider)))
	accountKey = strings.TrimSpace(accountKey)
	profileKey = strings.TrimSpace(profileKey)
	if provider == "" || accountKey == "" || profileKey == "" {
		return 0, identityfingerprint.AccountPolicy{}, fmt.Errorf("provider, account_key and profile_key are required")
	}
	db := getDB()
	if db == nil {
		return 0, identityfingerprint.AccountPolicy{}, fmt.Errorf("identity fingerprint store unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, identityfingerprint.AccountPolicy{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`DELETE FROM identity_fingerprints WHERE provider = ? AND account_key = ? AND profile_key = ?`, string(provider), accountKey, profileKey)
	if err != nil {
		return 0, identityfingerprint.AccountPolicy{}, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, identityfingerprint.AccountPolicy{}, err
	}
	policy, err := getIdentityFingerprintAccountPolicyWith(tx, provider, accountKey, currentUsageDriver() == "postgres")
	if errors.Is(err, sql.ErrNoRows) {
		policy = identityfingerprint.NormalizeAccountPolicy(provider, accountKey, identityfingerprint.AccountPolicy{})
	} else if err != nil {
		return 0, identityfingerprint.AccountPolicy{}, err
	}
	if policy.Strategy == identityfingerprint.AccountStrategyActiveProfile && policy.ActiveProfileKey == profileKey {
		policy.Strategy = identityfingerprint.AccountStrategyCLIPreferred
		policy.ActiveProfileKey = ""
		policy.Revision++
		if policy.Revision <= 0 {
			policy.Revision = 1
		}
		policy.UpdatedAt = time.Now().UTC()
		if err := upsertIdentityFingerprintAccountPolicyWith(tx, policy); err != nil {
			return 0, identityfingerprint.AccountPolicy{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, identityfingerprint.AccountPolicy{}, err
	}
	if deleted > 0 {
		notifyIdentityFingerprintInvalidated(provider, accountKey)
	}
	return deleted, policy, nil
}

type policyQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

type policyExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func getIdentityFingerprintAccountPolicyWith(queryer policyQueryer, provider identityfingerprint.Provider, accountKey string, forUpdate bool) (identityfingerprint.AccountPolicy, error) {
	query := `SELECT provider, account_key, strategy, active_profile_key, revision, updated_at
		FROM identity_fingerprint_account_policies
		WHERE provider = ? AND account_key = ?`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var policy identityfingerprint.AccountPolicy
	var providerText, strategy string
	var updatedAt storedTime
	if err := queryer.QueryRow(query, string(provider), accountKey).Scan(
		&providerText,
		&policy.AccountKey,
		&strategy,
		&policy.ActiveProfileKey,
		&policy.Revision,
		&updatedAt,
	); err != nil {
		return identityfingerprint.AccountPolicy{}, err
	}
	policy.Provider = identityfingerprint.Provider(providerText)
	policy.Strategy = identityfingerprint.AccountStrategy(strategy)
	if updatedAt.Valid {
		policy.UpdatedAt = updatedAt.Time
	}
	return identityfingerprint.NormalizeAccountPolicy(policy.Provider, policy.AccountKey, policy), nil
}

func ensureIdentityFingerprintAccountPolicyWith(execer policyExecer, provider identityfingerprint.Provider, accountKey string) error {
	_, err := execer.Exec(`
		INSERT INTO identity_fingerprint_account_policies (
			provider, account_key, strategy, active_profile_key, revision, updated_at
		) VALUES (?, ?, 'cli_preferred', '', 0, ?)
		ON CONFLICT(provider, account_key) DO NOTHING
	`, string(provider), accountKey, formatFingerprintTime(time.Now().UTC()))
	return err
}

func upsertIdentityFingerprintAccountPolicyWith(execer policyExecer, policy identityfingerprint.AccountPolicy) error {
	policy = identityfingerprint.NormalizeAccountPolicy(policy.Provider, policy.AccountKey, policy)
	_, err := execer.Exec(`
		INSERT INTO identity_fingerprint_account_policies (
			provider, account_key, strategy, active_profile_key, revision, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, account_key) DO UPDATE SET
			strategy = excluded.strategy,
			active_profile_key = excluded.active_profile_key,
			revision = excluded.revision,
			updated_at = excluded.updated_at
	`, string(policy.Provider), policy.AccountKey, string(policy.Strategy), policy.ActiveProfileKey, policy.Revision, formatFingerprintTime(policy.UpdatedAt))
	return err
}
