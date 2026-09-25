package dnswatch

import (
	"fmt"
	"strings"
	"time"
)

// Node tiers. With probe.egress_path set, a node is healthy when it passes
// both its readiness and its egress probe, degraded when it is ready but its
// egress check fails, and unhealthy when it is not ready. Without it, every
// ready node is healthy.
const (
	tierHealthy   = "healthy"
	tierDegraded  = "degraded"
	tierUnhealthy = "unhealthy"
)

// tier classifies s from its readiness and egress state machines.
func tier(s *nodeState) string {
	switch {
	case !s.healthy:
		return tierUnhealthy
	case s.egress != nil && !s.egress.healthy:
		return tierDegraded
	default:
		return tierHealthy
	}
}

// markDesired sets this round's DNS verdict on every node. DNS lists the
// healthy nodes when there are any and the degraded ones otherwise: a node
// that cannot reach its proxies leaves DNS while a peer still can, and an
// outage that degrades every node, such as the proxy provider itself being
// down, changes nothing. With the egress probe off, every flag stays clear.
func (w *Watcher) markDesired(anyHealthy bool) {
	for _, s := range w.nodes {
		s.sidelined, s.egressPending = false, false
		if s.egress == nil || !anyHealthy {
			continue
		}
		s.sidelined = s.healthy && !s.egress.healthy
		// A node whose egress passes only because DNS listed it at startup
		// keeps its records but is not added anywhere until a probe agrees.
		s.egressPending = !s.egress.confirmed
	}
}

// applyEgress feeds one egress probe result to a node and logs what it
// changed, the way apply does for readiness.
func (w *Watcher) applyEgress(s *nodeState, result probeResult, now time.Time) {
	e := s.egress
	wasOK := e.healthy
	obs := e.observe(result, now, w.limits)
	if !result.ok {
		w.counters.egressFailures++
	}
	switch {
	case obs.changed:
		e.deferNoted = false
		w.counters.stateChanges++
		w.reportEgressTransition(s)
	case !obs.deferredUntil.IsZero():
		if !e.deferNoted {
			e.deferNoted = true
			w.log.Info("egress state change held back by min_change_interval", "node", s.Name, "ip", s.IP,
				"egress_ok", e.healthy, "pending_egress_ok", !e.healthy, "until", obs.deferredUntil.UTC().Format(time.RFC3339),
				"consecutive_failures", e.failures, "consecutive_successes", e.successes)
		}
	default:
		e.deferNoted = false
		if wasOK && !result.ok {
			w.log.Warn("egress probe failed", "node", s.Name, "ip", s.IP, "path", w.cfg.Probe.EgressPath,
				"reason", result.reason, "consecutive_failures", e.failures, "fail_threshold", w.limits.fail)
		} else if !wasOK && result.ok {
			w.log.Info("egress probe succeeded while egress is marked failing", "node", s.Name, "ip", s.IP,
				"consecutive_successes", e.successes, "recover_threshold", w.limits.recover)
		}
	}
}

func (w *Watcher) reportEgressTransition(s *nodeState) {
	e := s.egress
	if e.healthy {
		w.log.Info("node egress recovered", "node", s.Name, "ip", s.IP,
			"reason", fmt.Sprintf("%d consecutive successful egress probes", e.successes),
			"consecutive_successes", e.successes, "consecutive_failures", e.failures)
		alert := w.newAlert("node_egress_recovered", fmt.Sprintf("node %s (%s) passes its egress check again after %d consecutive successful probes", s.Name, s.IP, e.successes))
		alert.Node, alert.IP, alert.ConsecutiveSuccesses = s.Name, s.IP, e.successes
		w.alerts.send(alert)
		return
	}
	w.log.Warn("node egress degraded", "node", s.Name, "ip", s.IP,
		"reason", e.lastFailure, "consecutive_failures", e.failures)
	alert := w.newAlert("node_egress_degraded", fmt.Sprintf("node %s (%s) fails its egress check after %d consecutive failed probes: %s", s.Name, s.IP, e.failures, e.lastFailure))
	alert.Node, alert.IP, alert.Reason, alert.ConsecutiveFailures = s.Name, s.IP, e.lastFailure, e.failures
	w.alerts.send(alert)
}

// trackAllDegraded logs entering and leaving the state where nodes are ready
// but none passes its egress check, in which DNS keeps every ready node.
func (w *Watcher) trackAllDegraded(healthy, degraded []string) {
	all := len(healthy) == 0 && len(degraded) > 0
	if all == w.allDegraded {
		return
	}
	w.allDegraded = all
	if all {
		w.log.Warn("no node passes its egress check; DNS keeps every ready node", "degraded", strings.Join(degraded, ","))
		return
	}
	if len(healthy) > 0 {
		w.log.Info("a node passes its egress check again; DNS follows the healthy nodes", "healthy", strings.Join(healthy, ","))
	}
}

// consecutiveFailures is the failure streak behind a removal: the egress
// check's for a sidelined node, readiness's otherwise.
func consecutiveFailures(s *nodeState) int {
	if s.sidelined {
		return s.egress.failures
	}
	return s.failures
}
