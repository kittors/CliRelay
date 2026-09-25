package clusterstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
)

// OAuthSessions is the oauth_sessions table, the shared oauthsession.Repo.
type OAuthSessions struct {
	db        *sql.DB
	lostAfter time.Duration
}

var _ oauthsession.Repo = (*OAuthSessions)(nil)

// NewOAuthSessions returns the repository over db.
func NewOAuthSessions(db *sql.DB) *OAuthSessions {
	return &OAuthSessions{db: db, lostAfter: oauthsession.OwnerLostAfter}
}

func seconds(d time.Duration) float64 { return d.Seconds() }

func (r *OAuthSessions) Create(ctx context.Context, rec oauthsession.Record, ttl time.Duration) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO oauth_sessions (
			state, provider, tenant_id, status, error, owner_node, callback_payload,
			created_at, expires_at, completed_at, heartbeat_at
		) VALUES (?, ?, ?, 'pending', '', ?, NULL, now(), now() + make_interval(secs => ?::double precision), NULL, now())
		ON CONFLICT (state) DO UPDATE SET
			provider = EXCLUDED.provider,
			tenant_id = EXCLUDED.tenant_id,
			status = 'pending',
			error = '',
			owner_node = EXCLUDED.owner_node,
			callback_payload = NULL,
			created_at = EXCLUDED.created_at,
			expires_at = EXCLUDED.expires_at,
			completed_at = NULL,
			heartbeat_at = EXCLUDED.heartbeat_at
	`, rec.State, rec.Provider, rec.TenantID, rec.OwnerNode, seconds(ttl))
	if err != nil {
		return fmt.Errorf("clusterstate: create oauth session: %w", err)
	}
	return nil
}

func (r *OAuthSessions) Get(ctx context.Context, state string) (oauthsession.Record, bool, error) {
	var rec oauthsession.Record
	err := r.db.QueryRowContext(ctx, `
		SELECT state, provider, tenant_id, status, error, owner_node, created_at, expires_at,
		       expires_at <= now(),
		       status = 'pending' AND heartbeat_at < now() - make_interval(secs => ?::double precision)
		  FROM oauth_sessions
		 WHERE state = ?
	`, seconds(r.lostAfter), state).Scan(
		&rec.State, &rec.Provider, &rec.TenantID, &rec.Status, &rec.Error, &rec.OwnerNode,
		&rec.CreatedAt, &rec.ExpiresAt, &rec.Expired, &rec.OwnerLost,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return oauthsession.Record{}, false, nil
	}
	if err != nil {
		return oauthsession.Record{}, false, fmt.Errorf("clusterstate: read oauth session: %w", err)
	}
	return rec, true, nil
}

func (r *OAuthSessions) Deliver(ctx context.Context, state, provider string, payload map[string]string) (bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("clusterstate: encode oauth callback: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE oauth_sessions
		   SET callback_payload = ?::jsonb,
		       expires_at = GREATEST(expires_at, now() + make_interval(secs => ?::double precision))
		 WHERE state = ?
		   AND provider = ?
		   AND status = 'pending'
		   AND expires_at > now()
		   AND heartbeat_at >= now() - make_interval(secs => ?::double precision)
	`, string(raw), seconds(oauthsession.CallbackGrace), state, provider, seconds(r.lostAfter))
	if err != nil {
		return false, fmt.Errorf("clusterstate: store oauth callback: %w", err)
	}
	return affected(res)
}

// TakeCallback reads and clears the payload in one statement. The row lock
// taken by FOR UPDATE makes a callback delivered concurrently either the one
// returned here or one left for the next read, never one cleared unseen.
func (r *OAuthSessions) TakeCallback(ctx context.Context, state string) (map[string]string, bool, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, `
		UPDATE oauth_sessions o
		   SET callback_payload = NULL
		  FROM (
			SELECT state, callback_payload
			  FROM oauth_sessions
			 WHERE state = ? AND status = 'pending' AND callback_payload IS NOT NULL
			   FOR UPDATE
		  ) taken
		 WHERE o.state = taken.state
		RETURNING taken.callback_payload::text
	`, state).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("clusterstate: take oauth callback: %w", err)
	}
	payload := map[string]string{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, fmt.Errorf("clusterstate: decode oauth callback: %w", err)
	}
	return payload, true, nil
}

func (r *OAuthSessions) Finish(ctx context.Context, state, status, message string, ttl time.Duration) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE oauth_sessions
		   SET status = ?, error = ?, callback_payload = NULL, completed_at = now(),
		       expires_at = now() + make_interval(secs => ?::double precision)
		 WHERE state = ? AND status = 'pending'
	`, status, message, seconds(ttl), state)
	if err != nil {
		return false, fmt.Errorf("clusterstate: finish oauth session: %w", err)
	}
	return affected(res)
}

func (r *OAuthSessions) CancelPending(ctx context.Context, provider, tenantID, message string, ttl time.Duration) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		UPDATE oauth_sessions
		   SET status = 'cancelled', error = ?, callback_payload = NULL, completed_at = now(),
		       expires_at = now() + make_interval(secs => ?::double precision)
		 WHERE provider = ?
		   AND status = 'pending'
		   AND expires_at > now()
		   AND (? = '' OR tenant_id = ?)
		RETURNING state
	`, message, seconds(ttl), provider, tenantID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("clusterstate: cancel oauth sessions: %w", err)
	}
	defer rows.Close()
	var states []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return nil, fmt.Errorf("clusterstate: cancel oauth sessions: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clusterstate: cancel oauth sessions: %w", err)
	}
	return states, nil
}

func (r *OAuthSessions) Heartbeat(ctx context.Context, state, owner string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE oauth_sessions
		   SET heartbeat_at = now()
		 WHERE state = ? AND owner_node = ? AND status = 'pending' AND expires_at > now()
	`, state, owner)
	if err != nil {
		return false, fmt.Errorf("clusterstate: heartbeat oauth session: %w", err)
	}
	return affected(res)
}

// oauthSessionRetention keeps finished and expired rows readable for a while,
// so a late status poll learns that the login expired rather than that the
// state never existed.
const oauthSessionRetention = time.Hour

// sweepOAuthSessions closes out logins nobody can finish and deletes rows past
// retention.
func sweepOAuthSessions(ctx context.Context, db *sql.DB, lostAfter time.Duration) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE oauth_sessions
		   SET status = 'expired', error = ?, callback_payload = NULL, completed_at = now()
		 WHERE status = 'pending' AND expires_at <= now()
	`, oauthsession.MessageExpired); err != nil {
		return fmt.Errorf("clusterstate: expire oauth sessions: %w", err)
	}
	// The PKCE verifier of a login lived only in its owner's process; once
	// that process stops heartbeating the login cannot finish anywhere.
	if _, err := db.ExecContext(ctx, `
		UPDATE oauth_sessions
		   SET status = 'error', error = ?, callback_payload = NULL, completed_at = now()
		 WHERE status = 'pending' AND heartbeat_at < now() - make_interval(secs => ?::double precision)
	`, oauthsession.MessageOwnerLost, seconds(lostAfter)); err != nil {
		return fmt.Errorf("clusterstate: close abandoned oauth sessions: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		DELETE FROM oauth_sessions WHERE expires_at < now() - make_interval(secs => ?::double precision)
	`, seconds(oauthSessionRetention)); err != nil {
		return fmt.Errorf("clusterstate: delete old oauth sessions: %w", err)
	}
	return nil
}

func affected(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
