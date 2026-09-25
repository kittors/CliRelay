package dnswatch

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Options carries the dependencies tests replace. The zero value is the
// production setup.
type Options struct {
	// Logger receives all output; nil logs text to stderr.
	Logger *slog.Logger
	// Now drives the state machine and summaries; nil uses time.Now.
	Now func() time.Time
	// RootCAs verifies node certificates; nil uses the system trust store.
	RootCAs *x509.CertPool
	// DialContext opens probe connections; nil dials the node directly.
	DialContext DialContextFunc
	// HTTPClient carries Cloudflare and webhook calls; nil uses a default
	// client, which honours HTTPS_PROXY for those egress calls.
	HTTPClient *http.Client
	// RetryBackoff is the first Cloudflare retry delay; zero means 1s.
	RetryBackoff time.Duration
}

// Watcher probes the nodes and reconciles DNS after every round.
// RunOnce and Run must not be called concurrently; Status may be.
type Watcher struct {
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
	dns    *cloudflareClient
	prober *prober
	alerts *alerter
	limits thresholds

	nodes            []*nodeState
	initialized      bool
	hold             bool
	holdProblem      string
	noHealthy        bool
	allDegraded      bool
	reconcileFailing bool
	records          map[string]recordView
	dryRunPlans      map[string]string
	lastReconcile    ReconcileStatus
	counters         summaryCounters
	lastSummary      time.Time
	startedAt        time.Time

	mu     sync.RWMutex
	status Status
}

type summaryCounters struct {
	rounds, probeFailures, egressFailures, stateChanges, dnsChanges, reconcileErrors int
}

// New builds a Watcher. cfg is copied and normalized, so configs built in
// code get the same defaults and validation as parsed ones.
func New(cfg *Config, token string, opts Options) (*Watcher, error) {
	if cfg == nil {
		return nil, errors.New("dnswatch: nil config")
	}
	c := *cfg
	c.Records = slices.Clone(cfg.Records)
	c.Nodes = slices.Clone(cfg.Nodes)
	c.Probe.ExpectStatus = slices.Clone(cfg.Probe.ExpectStatus)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("dnswatch: the Cloudflare API token is empty")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	w := &Watcher{
		cfg:         c,
		log:         logger,
		now:         now,
		dns:         newCloudflareClient(c.Cloudflare, token, httpClient, opts.RetryBackoff),
		prober:      newProber(c.Probe, opts.RootCAs, opts.DialContext),
		alerts:      newAlerter(c.AlertWebhook, httpClient, logger),
		limits:      thresholds{fail: c.Probe.FailThreshold, recover: c.Probe.RecoverThreshold, minChange: c.MinChangeInterval},
		records:     map[string]recordView{},
		dryRunPlans: map[string]string{},
		startedAt:   now(),
	}
	for _, node := range c.Nodes {
		s := &nodeState{Node: node}
		if c.Probe.EgressPath != "" {
			s.egress = &nodeState{Node: node}
		}
		w.nodes = append(w.nodes, s)
	}
	w.lastReconcile = ReconcileStatus{Skipped: "initializing"}
	w.publish()
	return w, nil
}

// Run probes and reconciles every probe.interval until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	if w.cfg.StatusListen != "" {
		stop, err := w.serveStatus(w.cfg.StatusListen)
		if err != nil {
			return err
		}
		defer stop()
	}
	defer w.Close()

	w.log.Info("dnswatch starting", w.cfg.LogAttrs()...)
	ticker := time.NewTicker(w.cfg.Probe.Interval)
	defer ticker.Stop()
	for {
		w.RunOnce(ctx)
		select {
		case <-ctx.Done():
			w.log.Info("dnswatch stopping")
			return nil
		case <-ticker.C:
		}
	}
}

// Close flushes queued alerts. Run calls it on the way out.
func (w *Watcher) Close() {
	w.alerts.close(alertDrainTimeout)
}

// RunOnce performs one probe round followed by DNS reconciliation.
func (w *Watcher) RunOnce(ctx context.Context) {
	defer w.publish()
	w.counters.rounds++
	if !w.initialized {
		if err := w.initialize(ctx); err != nil {
			if ctx.Err() == nil {
				w.log.Error("cannot read the current DNS records; DNS stays untouched until this succeeds", "error", err)
			}
			w.lastReconcile = ReconcileStatus{At: w.now(), Skipped: "initializing", Error: err.Error()}
			return
		}
	}

	results, egress := w.prober.probeRound(ctx, w.cfg.Nodes)
	if ctx.Err() != nil {
		// Shutting down: cancelled probes say nothing about the nodes.
		return
	}
	now := w.now()
	for i, s := range w.nodes {
		w.apply(s, results[i], now)
		if s.egress != nil {
			w.applyEgress(s, egress[i], now)
		}
	}
	w.checkHold()
	ready := w.readyNodes()
	w.trackNoHealthy(ready)
	healthy, degraded, _ := w.partitionNodes()
	w.trackAllDegraded(healthy, degraded)
	w.markDesired(len(healthy) > 0)

	switch {
	case w.hold:
		w.lastReconcile = ReconcileStatus{At: now, Skipped: "hold"}
	case len(ready) == 0:
		// Never empty the records: with every node failing its probe, the
		// likelier culprit is the arbiter's own network, and a stale record
		// still beats NXDOMAIN if any node can serve.
		w.lastReconcile = ReconcileStatus{At: now, Skipped: "no_healthy_nodes"}
	default:
		w.reconcile(ctx, now)
	}
	w.maybeLogSummary(now)
}

// initialize reads the published records once. Nodes already in DNS start
// healthy so a restart cannot pull them before real probes say otherwise;
// nodes missing from DNS must earn their place with recover_threshold probes.
func (w *Watcher) initialize(ctx context.Context) error {
	publishedIn := map[*nodeState][]string{}
	views := map[string]recordView{}
	for _, name := range w.cfg.Records {
		records, err := w.dns.listARecords(ctx, name)
		if err != nil {
			return err
		}
		owned, other := matchRecords(name, records, w.nodes)
		view := recordView{Other: other}
		for _, s := range w.nodes {
			if len(owned[s]) > 0 {
				publishedIn[s] = append(publishedIn[s], name)
				view.Nodes = append(view.Nodes, s.IP)
			}
		}
		views[name] = view
	}
	for _, s := range w.nodes {
		s.healthy = len(publishedIn[s]) > 0
		if s.egress != nil {
			// Egress starts from the same place: a published node keeps its
			// records until egress probes fail, a missing one earns them.
			s.egress.healthy = s.healthy
		}
		w.log.Info("initial node state from DNS", "node", s.Name, "ip", s.IP,
			"healthy", s.healthy, "published_in", strings.Join(publishedIn[s], ","))
	}
	w.records = views
	w.initialized = true
	return nil
}

// apply feeds one probe result to a node and logs what it changed.
func (w *Watcher) apply(s *nodeState, result probeResult, now time.Time) {
	wasHealthy := s.healthy
	obs := s.observe(result, now, w.limits)
	if !result.ok {
		w.counters.probeFailures++
	}
	switch {
	case obs.changed:
		s.deferNoted = false
		w.counters.stateChanges++
		w.reportTransition(s)
	case !obs.deferredUntil.IsZero():
		if !s.deferNoted {
			s.deferNoted = true
			w.log.Info("state change held back by min_change_interval", "node", s.Name, "ip", s.IP,
				"healthy", s.healthy, "pending_healthy", !s.healthy, "until", obs.deferredUntil.UTC().Format(time.RFC3339),
				"consecutive_failures", s.failures, "consecutive_successes", s.successes)
		}
	default:
		s.deferNoted = false
		// Progress toward a flip is worth a line; the repeated failures of a
		// node that is already down are left to the periodic summary.
		if wasHealthy && !result.ok {
			w.log.Warn("probe failed", "node", s.Name, "ip", s.IP, "reason", result.reason,
				"consecutive_failures", s.failures, "fail_threshold", w.limits.fail)
		} else if !wasHealthy && result.ok {
			w.log.Info("probe succeeded on unhealthy node", "node", s.Name, "ip", s.IP,
				"consecutive_successes", s.successes, "recover_threshold", w.limits.recover)
		}
	}
}

func (w *Watcher) reportTransition(s *nodeState) {
	if s.healthy {
		w.log.Info("node marked healthy", "node", s.Name, "ip", s.IP,
			"reason", fmt.Sprintf("%d consecutive successful probes", s.successes),
			"consecutive_successes", s.successes, "consecutive_failures", s.failures)
		alert := w.newAlert("node_healthy", fmt.Sprintf("node %s (%s) is healthy again after %d consecutive successful probes", s.Name, s.IP, s.successes))
		alert.Node, alert.IP, alert.ConsecutiveSuccesses = s.Name, s.IP, s.successes
		w.alerts.send(alert)
		return
	}
	w.log.Warn("node marked unhealthy", "node", s.Name, "ip", s.IP,
		"reason", s.lastFailure, "consecutive_failures", s.failures)
	alert := w.newAlert("node_unhealthy", fmt.Sprintf("node %s (%s) is unhealthy after %d consecutive failed probes: %s", s.Name, s.IP, s.failures, s.lastFailure))
	alert.Node, alert.IP, alert.Reason, alert.ConsecutiveFailures = s.Name, s.IP, s.lastFailure, s.failures
	w.alerts.send(alert)
}

// checkHold refreshes the hold flag from hold_file.
func (w *Watcher) checkHold() {
	path := w.cfg.HoldFile
	hold, problem := false, ""
	if path != "" {
		_, err := os.Stat(path)
		switch {
		case err == nil:
			hold = true
		case errors.Is(err, fs.ErrNotExist):
		default:
			// Unable to tell whether an operator asked for a hold: freeze DNS
			// rather than guess.
			hold, problem = true, err.Error()
		}
	}
	if hold == w.hold && problem == w.holdProblem {
		return
	}
	w.hold, w.holdProblem = hold, problem
	switch {
	case problem != "":
		w.log.Warn("cannot check the hold file; DNS changes are suspended", "hold_file", path, "error", problem)
	case hold:
		w.log.Warn("hold file present; probing continues but DNS changes are suspended", "hold_file", path)
	default:
		w.log.Info("hold file gone; DNS reconciliation resumes", "hold_file", path)
	}
	event, text := "dns_hold_ended", "hold file removed; DNS reconciliation resumes"
	if hold {
		event, text = "dns_hold_started", "DNS changes suspended by hold file "+path
	}
	w.alerts.send(w.newAlert(event, text))
}

// trackNoHealthy logs and alerts on entering and leaving the state where no
// node passes its probe.
func (w *Watcher) trackNoHealthy(healthy []string) {
	none := len(healthy) == 0
	if none == w.noHealthy {
		return
	}
	w.noHealthy = none
	if none {
		w.log.Error("no healthy nodes; keeping every DNS record as it is", "nodes", w.nodeSummary())
		w.alerts.send(w.newAlert("no_healthy_nodes", "no node is healthy; DNS records are left unchanged"))
		return
	}
	w.log.Info("healthy nodes available again", "healthy", strings.Join(healthy, ","))
	w.alerts.send(w.newAlert("no_healthy_nodes_resolved", "healthy nodes available again: "+strings.Join(healthy, ", ")))
}

func (w *Watcher) maybeLogSummary(now time.Time) {
	if w.lastSummary.IsZero() {
		w.lastSummary = now
		return
	}
	if now.Sub(w.lastSummary) < w.cfg.SummaryInterval {
		return
	}
	healthy, degraded, unhealthy := w.partitionNodes()
	c := w.counters
	egressOn := w.cfg.Probe.EgressPath != ""
	attrs := []any{"healthy", strings.Join(healthy, ",")}
	if egressOn {
		attrs = append(attrs, "degraded", strings.Join(degraded, ","))
	}
	attrs = append(attrs,
		"unhealthy", strings.Join(unhealthy, ","),
		"records", w.recordsSummary(),
		"rounds", c.rounds,
		"probe_failures", c.probeFailures)
	if egressOn {
		attrs = append(attrs, "egress_failures", c.egressFailures)
	}
	attrs = append(attrs,
		"state_changes", c.stateChanges,
		"dns_changes", c.dnsChanges,
		"reconcile_errors", c.reconcileErrors,
		"hold", w.hold,
		"dry_run", w.cfg.DryRun)
	w.log.Info("dnswatch summary", attrs...)
	w.counters = summaryCounters{}
	w.lastSummary = now
}

// partitionNodes returns node names by tier, in config order. degraded stays
// empty while the egress probe is off.
func (w *Watcher) partitionNodes() (healthy, degraded, unhealthy []string) {
	healthy, degraded, unhealthy = []string{}, []string{}, []string{}
	for _, s := range w.nodes {
		switch tier(s) {
		case tierHealthy:
			healthy = append(healthy, s.Name)
		case tierDegraded:
			degraded = append(degraded, s.Name)
		default:
			unhealthy = append(unhealthy, s.Name)
		}
	}
	return healthy, degraded, unhealthy
}

// readyNodes returns the names of the nodes that pass their readiness probe,
// healthy or degraded, in config order.
func (w *Watcher) readyNodes() []string {
	ready := []string{}
	for _, s := range w.nodes {
		if s.healthy {
			ready = append(ready, s.Name)
		}
	}
	return ready
}

func (w *Watcher) nodeSummary() string {
	parts := make([]string, 0, len(w.nodes))
	for _, s := range w.nodes {
		reason := s.lastFailure
		if s.last.ok {
			reason = "ok"
		}
		parts = append(parts, fmt.Sprintf("%s(%s)=%s", s.Name, s.IP, reason))
	}
	return strings.Join(parts, " ")
}

func (w *Watcher) recordsSummary() string {
	parts := make([]string, 0, len(w.cfg.Records))
	for _, name := range w.cfg.Records {
		parts = append(parts, name+"=["+strings.Join(w.records[name].Nodes, " ")+"]")
	}
	return strings.Join(parts, " ")
}

func (w *Watcher) newAlert(event, text string) Alert {
	healthy, degraded, unhealthy := w.partitionNodes()
	return Alert{
		Event:          event,
		Text:           text,
		Time:           w.now().UTC(),
		HealthyNodes:   healthy,
		DegradedNodes:  degraded,
		UnhealthyNodes: unhealthy,
		DryRun:         w.cfg.DryRun,
		Hold:           w.hold,
	}
}
