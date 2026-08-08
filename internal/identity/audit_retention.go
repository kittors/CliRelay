package identity

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// Audit retention.
//
// audit_logs had no lifecycle at all: rows accumulated for the life of the
// deployment, and the only way to shrink the table was the panel's "clear all"
// button, which throws away the entire trail. A write policy that only records
// security-relevant events (see the management middleware) keeps the growth rate
// sane, but an audit table still needs a ceiling — the point of a bound is that
// it holds even when a future caller starts recording something noisy.
//
// Both limits are deliberately generous. Retention is what an operator answers
// compliance questions with; the row cap exists to stop a runaway, not to trim
// normal usage, and it is only reached if something is writing far more than the
// current policy allows.
const (
	defaultAuditRetention        = 180 * 24 * time.Hour
	defaultAuditMaxRows          = int64(200000)
	defaultAuditRetentionBatch   = 500
	defaultAuditRetentionCadence = time.Hour
)

// AuditRetentionPolicy bounds the audit trail by age and by row count.
type AuditRetentionPolicy struct {
	// MaxAge drops rows older than this. Zero disables the age limit.
	MaxAge time.Duration
	// MaxRows keeps only the newest rows once the table exceeds it. Zero
	// disables the cap.
	MaxRows int64
	// BatchSize caps rows deleted per statement so a pass never holds a long
	// write lock on the table the middleware writes to.
	BatchSize int
	// Interval is the cadence of the background pass.
	Interval time.Duration
}

func (p AuditRetentionPolicy) normalized() AuditRetentionPolicy {
	if p.MaxAge < 0 {
		p.MaxAge = 0
	}
	if p.MaxRows < 0 {
		p.MaxRows = 0
	}
	if p.BatchSize <= 0 {
		p.BatchSize = defaultAuditRetentionBatch
	}
	if p.Interval <= 0 {
		p.Interval = defaultAuditRetentionCadence
	}
	return p
}

// DefaultAuditRetentionPolicy is used when the operator configured nothing.
func DefaultAuditRetentionPolicy() AuditRetentionPolicy {
	return AuditRetentionPolicy{
		MaxAge:    defaultAuditRetention,
		MaxRows:   defaultAuditMaxRows,
		BatchSize: defaultAuditRetentionBatch,
		Interval:  defaultAuditRetentionCadence,
	}
}

// AuditRetentionPolicyFromConfig resolves the policy, defaulting per field so an
// operator who sets only one of them still gets the shipped value for the other.
// A negative value disables that limit explicitly.
func AuditRetentionPolicyFromConfig(cfg *config.Config) AuditRetentionPolicy {
	policy := DefaultAuditRetentionPolicy()
	if cfg == nil {
		return policy
	}
	auth := cfg.RemoteManagement.Auth
	if days := auth.AuditRetentionDays; days != 0 {
		if days < 0 {
			policy.MaxAge = 0
		} else {
			policy.MaxAge = time.Duration(days) * 24 * time.Hour
		}
	}
	if rows := auth.AuditMaxRows; rows != 0 {
		if rows < 0 {
			policy.MaxRows = 0
		} else {
			policy.MaxRows = int64(rows)
		}
	}
	return policy
}

// StartAuditRetention runs the retention pass on a ticker until the returned stop
// function is called. Like the session reaper, the stop function must be wired
// into process shutdown so a re-initialised runtime stack does not leak the
// goroutine.
func (s *Service) StartAuditRetention(ctx context.Context, policy AuditRetentionPolicy) (stop func()) {
	if s == nil || s.db == nil {
		return func() {}
	}
	policy = policy.normalized()
	if policy.MaxAge == 0 && policy.MaxRows == 0 {
		log.Debug("identity: audit retention disabled by configuration")
		return func() {}
	}
	retentionCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(policy.Interval)
		defer ticker.Stop()
		// Run once at startup so an existing oversized table is brought inside the
		// policy without waiting a full interval.
		s.runAuditRetentionPass(retentionCtx, policy)
		for {
			select {
			case <-retentionCtx.Done():
				return
			case <-ticker.C:
				s.runAuditRetentionPass(retentionCtx, policy)
			}
		}
	}()
	return cancel
}

func (s *Service) runAuditRetentionPass(ctx context.Context, policy AuditRetentionPolicy) {
	deleted, err := s.PruneAuditLogs(ctx, policy)
	switch {
	case err != nil && ctx.Err() != nil:
		return
	case err != nil:
		log.WithError(err).Warn("identity: audit retention pass failed")
	case deleted > 0:
		log.Infof("identity: audit retention removed %d audit_logs rows", deleted)
	default:
		log.Debug("identity: audit retention removed 0 audit_logs rows")
	}
}

// PruneAuditLogs enforces the policy once and returns how many rows it removed.
// Age is enforced first so the row cap only ever has to deal with rows an
// operator still wants to keep.
func (s *Service) PruneAuditLogs(ctx context.Context, policy AuditRetentionPolicy) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	policy = policy.normalized()
	var total int64

	if policy.MaxAge > 0 {
		cutoff := time.Now().UTC().Add(-policy.MaxAge)
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			res, err := s.db.ExecContext(ctx, `
				DELETE FROM audit_logs
				 WHERE id IN (SELECT id FROM audit_logs WHERE created_at < ? ORDER BY id LIMIT ?)
			`, cutoff, policy.BatchSize)
			if err != nil {
				return total, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return total, err
			}
			total += n
			if n < int64(policy.BatchSize) {
				break
			}
		}
	}

	if policy.MaxRows > 0 {
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			var count int64
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&count); err != nil {
				return total, err
			}
			if count <= policy.MaxRows {
				break
			}
			// The id of the oldest row still inside the cap; everything below it goes.
			var cutoffID int64
			if err := s.db.QueryRowContext(ctx,
				`SELECT id FROM audit_logs ORDER BY id DESC LIMIT 1 OFFSET ?`, policy.MaxRows-1).Scan(&cutoffID); err != nil {
				return total, err
			}
			res, err := s.db.ExecContext(ctx, `
				DELETE FROM audit_logs
				 WHERE id IN (SELECT id FROM audit_logs WHERE id < ? ORDER BY id LIMIT ?)
			`, cutoffID, policy.BatchSize)
			if err != nil {
				return total, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return total, err
			}
			total += n
			if n == 0 {
				break
			}
		}
	}

	return total, nil
}
