package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

// ─── Responses WebSocket ────────────────────────────────────────────────────
// GET /responses is also the Responses WebSocket upgrade. One connection
// carries many turns, and each turn reaches the executor and spends upstream
// quota exactly like a POST /responses. The upgrade request carries no turn, so
// the checks are split: the handshake is screened here, and every turn is
// admitted by the gate published for the frame loop.

// admitWebsocketHandshake screens a WebSocket upgrade and, when the key has
// limits, publishes the per-turn gate.
//
// A handshake opens a connection; it is not a request and never reaches an
// executor. So it is not counted toward RPM and does not take a concurrency
// slot: counting it would charge the connection's first turn twice, and a slot
// held for the connection's lifetime would leave a key with concurrency-limit 1
// no slot for its own turns. The throttles that describe the moment of a
// request (concurrency, RPM, TPM) are left to each turn, since they can clear
// before the first turn arrives. Only an exhausted budget refuses the
// handshake — every turn on the connection would be refused anyway — and the
// refusal is the response a POST gets.
func admitWebsocketHandshake(c *gin.Context) {
	apiKey, metadata, ok := quotaCredentials(c)
	if !ok || metadata == nil {
		c.Next()
		return
	}
	policy := parseQuotaPolicy(apiKey, metadata)
	if !policy.hasLimits() {
		// No gate is published, so turns of keys without limits skip quota
		// entirely, as they always have.
		c.Next()
		return
	}
	policy.record(c)
	if verdict := policy.checkBudgets(); verdict != nil {
		verdict.abort(c)
		return
	}
	c.Set(handlers.QuotaGateContextKey, policy.turnGate())
	c.Next()
}

// turnGate admits one WebSocket turn with the full POST checks. The limits are
// the ones in force at the handshake; usage is read fresh for every turn. Like a
// POST, the turn counts toward RPM whether or not it is admitted.
func (p quotaPolicy) turnGate() handlers.QuotaGate {
	return func() (func(), *handlers.QuotaRejection) {
		getRPMTracker(p.subject).add()
		release, verdict := p.admit()
		if verdict != nil {
			return nil, verdict.forTurn()
		}
		return release, nil
	}
}
