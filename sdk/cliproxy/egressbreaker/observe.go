package egressbreaker

import (
	"strings"
	"time"
)

// The read side of the breaker, for the console and the alert rules: what is
// known about a proxy, asked without counting as a request.

// Code is the fault's stable name in APIs and alert payloads, empty for
// FaultNone. String is the wording for people; this does not change with it.
func (f Fault) Code() string {
	switch f {
	case FaultAuthRejected:
		return "auth_rejected"
	case FaultUnreachable:
		return "unreachable"
	default:
		return ""
	}
}

// Enabled reports whether the breaker is on (see EnvSwitch). Off, nothing is
// ever refused or recorded, so every Observation is empty.
func (b *Breaker) Enabled() bool {
	return b != nil && !b.disabled.Load()
}

// Observation is what the breaker knows about one proxy.
type Observation struct {
	// Open says requests through the proxy are being refused; Status then
	// describes the outage.
	Open bool
	Status
	// Probing says the request let through to try the proxy is still out.
	Probing bool
	// LastPass is when a request last got through the proxy in this process,
	// zero when none has within staleAfter. A pass during an outage is only
	// recorded for the probe that ended it, so a LastPass after an outage
	// began means that outage is over, which "not open" alone cannot say for
	// a process that started after the outage did.
	LastPass time.Time
}

// Observe reports what the breaker knows about proxyURL. Unlike Check it is
// not a request: it neither marks the proxy as asked about, which would keep
// a forgotten proxy's state alive, nor refuses anything, so a console that
// polls it changes nothing.
func (b *Breaker) Observe(proxyURL string) Observation {
	key := strings.TrimSpace(proxyURL)
	if b == nil || key == "" || b.disabled.Load() {
		return Observation{}
	}
	now := b.cfg.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out Observation
	if at, ok := b.passed[key]; ok && now.Sub(at) <= staleAfter {
		out.LastPass = at
	}
	s := b.states[key]
	// A state nothing has asked about for staleAfter is one Open would drop:
	// a proxy replaced in the pool leaves its old URL behind, open for good.
	if s == nil || !s.open || now.Sub(s.lastSeen) > staleAfter {
		return out
	}
	out.Open = true
	out.Status = s.status()
	out.Probing = s.probing && now.Sub(s.probeStarted) < b.cfg.ProbeTimeout
	return out
}

// status describes an open state. The caller holds b.mu.
func (s *state) status() Status {
	return Status{
		Label: s.label, Fault: s.fault, Since: s.since, NextProbe: s.until,
		Refused: s.refused, FailedProbes: s.failedProbes,
	}
}
