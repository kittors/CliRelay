package middleware

import (
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
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
	rememberAdmissionSubject(apiKey, policy.subject)
	if !policy.hasLimits() {
		// Turns of keys without limits skip quota entirely, as they always
		// have. A key whose only limits are per-model rules gets a gate that
		// admits every turn and only holds its reservation, for the model
		// policy gate to count.
		if policy.hasModelRules() {
			policy.requestID = ensureRequestLoggingRequestID(c)
			c.Set(handlers.QuotaGateContextKey, policy.reservationTurnGate(publishTurnReservations(c)))
		}
		c.Next()
		return
	}
	policy.record(c)
	if verdict := policy.checkBudgets(); verdict != nil {
		verdict.abort(c)
		return
	}
	// Every turn's usage records carry the connection's request id; the ledger
	// tells the turns' records apart by the order they arrive in.
	policy.requestID = ensureRequestLoggingRequestID(c)
	c.Set(handlers.QuotaGateContextKey, policy.turnGate(publishTurnReservations(c)))
	c.Next()
}

// turnGate admits one WebSocket turn with the full POST checks. The limits are
// the ones in force at the handshake; usage is read fresh for every turn. Like a
// POST, the turn counts toward RPM whether or not it is admitted.
func (p quotaPolicy) turnGate(turns *turnReservations) handlers.QuotaGate {
	return func() (func(), *handlers.QuotaRejection) {
		turn := p
		// 每一轮都要读到最新用量，不能沿用握手或上一轮的快照。
		if turn.access != nil {
			turn.usage = newPolicyUsage(turn.access)
		}
		turn.countRequest()
		release, verdict := turn.admit()
		if verdict != nil {
			turns.put(nil)
			return nil, verdict.forTurn()
		}
		turns.put(turn.inflight)
		return release, nil
	}
}

// reservationTurnGate admits every turn of a key whose only limits are
// per-model rules. Those are checked by the model policy gate; the reservation
// held here lets that check see the key's other requests in flight.
func (p quotaPolicy) reservationTurnGate(turns *turnReservations) handlers.QuotaGate {
	return func() (func(), *handlers.QuotaRejection) {
		turn := p
		reservation := turn.reserve()
		turns.put(reservation)
		return reservation.Finish, nil
	}
}

// turnReservations hands the reservation of the turn the quota gate has just
// admitted to the model policy gate, which the frame loop runs next for the
// same turn.
type turnReservations struct {
	current atomic.Pointer[inflight.Reservation]
}

const quotaTurnReservationsContextKey = "cliproxy.quotaTurnReservations"

func publishTurnReservations(c *gin.Context) *turnReservations {
	turns := &turnReservations{}
	c.Set(quotaTurnReservationsContextKey, turns)
	return turns
}

func turnReservationsFromContext(c *gin.Context) *turnReservations {
	value, _ := c.Get(quotaTurnReservationsContextKey)
	turns, _ := value.(*turnReservations)
	return turns
}

func (t *turnReservations) put(r *inflight.Reservation) {
	if t != nil {
		t.current.Store(r)
	}
}

// take returns the reservation of the turn being gated, once: a turn the quota
// gate did not admit must not pick up the previous turn's.
func (t *turnReservations) take() *inflight.Reservation {
	if t == nil {
		return nil
	}
	return t.current.Swap(nil)
}
