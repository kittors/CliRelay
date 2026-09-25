package dnswatch

import "time"

// thresholds are the state machine settings shared by every node.
type thresholds struct {
	fail      int
	recover   int
	minChange time.Duration
}

// nodeState tracks one node's health as seen by consecutive probes.
type nodeState struct {
	Node
	healthy bool
	// confirmed is set by the first successful probe. A node starts out
	// healthy when DNS already lists it, which avoids pulling it at startup;
	// until a probe confirms it, dnswatch keeps its existing records but does
	// not publish it under any record that lacks it.
	confirmed bool
	failures  int
	successes int
	// lastChange is when healthy last flipped. It is zero until the first
	// flip, so the state inferred from DNS never delays the first change.
	lastChange  time.Time
	lastProbeAt time.Time
	last        probeResult
	// lastFailure keeps the most recent failure reason for DNS change logs.
	lastFailure string
	// deferNoted keeps the "held by min_change_interval" line to one per hold.
	deferNoted bool
}

// observation reports what one probe result did to a node.
type observation struct {
	changed bool
	// deferredUntil is set when a threshold was reached but
	// min_change_interval postponed the flip until then.
	deferredUntil time.Time
}

// observe folds one probe result into the node's counters. The node flips
// once the failure (or recovery) streak reaches its threshold, unless it
// flipped less than minChange ago; the streak keeps counting meanwhile, so
// the flip happens on the first probe after the interval if nothing broke it.
func (s *nodeState) observe(result probeResult, now time.Time, t thresholds) observation {
	s.last = result
	s.lastProbeAt = now
	if result.ok {
		s.successes++
		s.failures = 0
		s.confirmed = true
		if s.healthy || s.successes < t.recover {
			return observation{}
		}
	} else {
		s.failures++
		s.successes = 0
		s.lastFailure = result.reason
		if !s.healthy || s.failures < t.fail {
			return observation{}
		}
	}
	if !s.lastChange.IsZero() {
		if next := s.lastChange.Add(t.minChange); now.Before(next) {
			return observation{deferredUntil: next}
		}
	}
	s.healthy = !s.healthy
	s.lastChange = now
	return observation{changed: true}
}
