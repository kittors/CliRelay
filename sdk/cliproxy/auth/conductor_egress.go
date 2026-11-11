package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// The scheduler's side of the egress breaker (package egressbreaker).
//
// The breaker itself sits in the upstream transports, where it sees every
// connection. That alone is too late for the scheduler: an executor translates
// the request body before it opens a connection, bodies are often several
// megabytes, and a request walks every account of the pool before it gives up.
// With a proxy down, each incoming request would still translate its body once
// per account only to be refused at the transport. So the scheduler asks the
// breaker before it calls the executor and skips the account when its proxy is
// known to be out.
//
// A proxy failure is not the account's failure. It starts no cooldown and
// leaves no error on the account, which would otherwise show a working
// account as broken in the console for as long as its proxy is down.

// EgressUnavailableCode is the Error.Code of a failure caused by the proxy an
// account's traffic leaves through, as opposed to the account or the upstream.
const EgressUnavailableCode = "egress_unavailable"

// EgressProxyResolver is implemented by executors whose upstream traffic for a
// credential leaves through a configured proxy. EgressProxyURL returns that
// proxy's URL, or "" when the credential's traffic goes out directly.
//
// The executor answers rather than the scheduler working it out, because the
// executor holds the tenant configuration it was bound with and resolves the
// credential's proxy-pool entry against it. The value must be the one the
// executor's own requests use: the breaker keys its state by it.
type EgressProxyResolver interface {
	EgressProxyURL(auth *Auth) string
}

// EgressProxyURL returns the proxy a credential's upstream traffic leaves
// through, as the executor the scheduler picks for it resolves the proxy: the
// key the egress breaker keeps its state under. Empty means the traffic goes
// out directly, or that the executor cannot name one proxy for all of its
// requests. It is for readers outside the request path (the console, the
// alert rules) that need to tie a breaker state back to accounts.
func (m *Manager) EgressProxyURL(auth *Auth) string {
	if m == nil || auth == nil {
		return ""
	}
	providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
	m.mu.RLock()
	executor := m.executorForAuthLocked(auth, providerKey)
	m.mu.RUnlock()
	resolver, ok := executor.(EgressProxyResolver)
	if !ok {
		return ""
	}
	return strings.TrimSpace(resolver.EgressProxyURL(auth))
}

func (m *Manager) egressBreaker() *egressbreaker.Breaker {
	if m != nil && m.egress != nil {
		return m.egress
	}
	return egressbreaker.Default()
}

// egressRefusal returns the error a candidate fails with when the proxy its
// account leaves through is known not to work, nil when the executor may be
// called. It does not claim the breaker's probe: the executor's transport does
// that when it really connects.
func (s executionService) egressRefusal(candidate *mixedExecutionCandidate) error {
	resolver, ok := candidate.executor.(EgressProxyResolver)
	if !ok {
		return nil
	}
	return s.manager.egressBreaker().Check(resolver.EgressProxyURL(candidate.auth))
}

// isEgressUnavailable reports whether a failed result was caused by the proxy
// rather than by the account.
func isEgressUnavailable(err *Error) bool {
	return err != nil && err.Code == EgressUnavailableCode
}
