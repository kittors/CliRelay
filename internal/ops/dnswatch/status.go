package dnswatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"time"
)

// Status is the JSON document served at GET /status. DegradedNodes counts
// the ready nodes failing their egress check and stays 0 while the egress
// probe is off.
type Status struct {
	StartedAt     time.Time               `json:"started_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	Initialized   bool                    `json:"initialized"`
	DryRun        bool                    `json:"dry_run"`
	Hold          bool                    `json:"hold"`
	HealthyNodes  int                     `json:"healthy_nodes"`
	DegradedNodes int                     `json:"degraded_nodes"`
	Nodes         []NodeStatus            `json:"nodes"`
	Records       map[string]RecordStatus `json:"records"`
	LastReconcile ReconcileStatus         `json:"last_reconcile"`
}

// NodeStatus is one node's state as of the last round. Healthy and the
// counters are the readiness state machine. State is the node's tier:
// healthy, degraded (ready, egress check failing) or unhealthy (not ready).
// Egress, the egress check's state machine, is present only with
// probe.egress_path set.
type NodeStatus struct {
	Name                 string     `json:"name"`
	IP                   string     `json:"ip"`
	Healthy              bool       `json:"healthy"`
	State                string     `json:"state"`
	Confirmed            bool       `json:"confirmed"`
	ConsecutiveFailures  int        `json:"consecutive_failures"`
	ConsecutiveSuccesses int        `json:"consecutive_successes"`
	LastChange           *time.Time `json:"last_change,omitempty"`
	LastProbe            *time.Time `json:"last_probe,omitempty"`
	LastStatus           int        `json:"last_status,omitempty"`
	LastLatencyMS        int64      `json:"last_latency_ms"`
	LastError            string     `json:"last_error,omitempty"`

	Egress *EgressStatus `json:"egress,omitempty"`
}

// EgressStatus is one node's egress state machine as of the last round.
type EgressStatus struct {
	OK                   bool       `json:"ok"`
	Confirmed            bool       `json:"confirmed"`
	ConsecutiveFailures  int        `json:"consecutive_failures"`
	ConsecutiveSuccesses int        `json:"consecutive_successes"`
	LastChange           *time.Time `json:"last_change,omitempty"`
	LastProbe            *time.Time `json:"last_probe,omitempty"`
	LastStatus           int        `json:"last_status,omitempty"`
	LastLatencyMS        int64      `json:"last_latency_ms"`
	LastError            string     `json:"last_error,omitempty"`
}

// RecordStatus is what dnswatch last saw published under one hostname.
type RecordStatus struct {
	Nodes []string `json:"nodes"`
	Other []string `json:"other,omitempty"`
}

// ReconcileStatus describes the most recent reconciliation attempt.
type ReconcileStatus struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	// Skipped names why DNS was left alone: initializing, hold or
	// no_healthy_nodes.
	Skipped string `json:"skipped,omitempty"`
}

// Status returns a copy of the state published after the last round.
func (w *Watcher) Status() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return cloneStatus(w.status)
}

// StatusHandler serves GET /status.
func (w *Watcher) StatusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Cache-Control", "no-store")
		encoder := json.NewEncoder(rw)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(w.Status())
	})
	return mux
}

// serveStatus starts the status listener and returns its shutdown function.
func (w *Watcher) serveStatus(addr string) (func(), error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("status_listen %s: %w", addr, err)
	}
	server := &http.Server{
		Handler:           w.StatusHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.log.Error("status endpoint stopped", "error", err)
		}
	}()
	w.log.Info("status endpoint listening", "addr", listener.Addr().String())
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}, nil
}

// publish snapshots the loop-owned state for concurrent Status readers.
func (w *Watcher) publish() {
	snapshot := Status{
		StartedAt:     w.startedAt.UTC(),
		UpdatedAt:     w.now().UTC(),
		Initialized:   w.initialized,
		DryRun:        w.cfg.DryRun,
		Hold:          w.hold,
		Nodes:         make([]NodeStatus, 0, len(w.nodes)),
		Records:       make(map[string]RecordStatus, len(w.records)),
		LastReconcile: w.lastReconcile,
	}
	if !snapshot.LastReconcile.At.IsZero() {
		snapshot.LastReconcile.At = snapshot.LastReconcile.At.UTC()
	}
	for _, s := range w.nodes {
		node := NodeStatus{
			Name:                 s.Name,
			IP:                   s.IP,
			Healthy:              s.healthy,
			State:                tier(s),
			Confirmed:            s.confirmed,
			ConsecutiveFailures:  s.failures,
			ConsecutiveSuccesses: s.successes,
			LastChange:           optionalTime(s.lastChange),
			LastProbe:            optionalTime(s.lastProbeAt),
			LastStatus:           s.last.status,
			LastLatencyMS:        s.last.latency.Milliseconds(),
			LastError:            s.last.reason,
		}
		if e := s.egress; e != nil {
			node.Egress = &EgressStatus{
				OK:                   e.healthy,
				Confirmed:            e.confirmed,
				ConsecutiveFailures:  e.failures,
				ConsecutiveSuccesses: e.successes,
				LastChange:           optionalTime(e.lastChange),
				LastProbe:            optionalTime(e.lastProbeAt),
				LastStatus:           e.last.status,
				LastLatencyMS:        e.last.latency.Milliseconds(),
				LastError:            e.last.reason,
			}
		}
		switch node.State {
		case tierHealthy:
			snapshot.HealthyNodes++
		case tierDegraded:
			snapshot.DegradedNodes++
		}
		snapshot.Nodes = append(snapshot.Nodes, node)
	}
	for name, view := range w.records {
		snapshot.Records[name] = RecordStatus{
			Nodes: append([]string{}, view.Nodes...),
			Other: slices.Clone(view.Other),
		}
	}
	w.mu.Lock()
	w.status = snapshot
	w.mu.Unlock()
}

func cloneStatus(s Status) Status {
	s.Nodes = slices.Clone(s.Nodes)
	for i := range s.Nodes {
		s.Nodes[i].LastChange = cloneTime(s.Nodes[i].LastChange)
		s.Nodes[i].LastProbe = cloneTime(s.Nodes[i].LastProbe)
		if egress := s.Nodes[i].Egress; egress != nil {
			copied := *egress
			copied.LastChange = cloneTime(egress.LastChange)
			copied.LastProbe = cloneTime(egress.LastProbe)
			s.Nodes[i].Egress = &copied
		}
	}
	s.Records = maps.Clone(s.Records)
	for name, record := range s.Records {
		s.Records[name] = RecordStatus{Nodes: slices.Clone(record.Nodes), Other: slices.Clone(record.Other)}
	}
	return s
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	copied := *t
	return &copied
}
