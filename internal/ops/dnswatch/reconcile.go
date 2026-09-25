package dnswatch

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// recordView is what dnswatch last saw published under one hostname.
type recordView struct {
	// Nodes are the IPs of configured nodes with an A record here.
	Nodes []string
	// Other are A records outside the node list; dnswatch never touches them.
	Other []string
}

// DNSChange is one record dnswatch added or removed.
type DNSChange struct {
	Action              string `json:"action"`
	Domain              string `json:"domain"`
	Node                string `json:"node"`
	IP                  string `json:"ip"`
	RecordID            string `json:"record_id,omitempty"`
	Reason              string `json:"reason"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// domainPlan lists the changes one hostname needs to match node health.
type domainPlan struct {
	name string
	// add holds healthy, confirmed nodes that have no record here yet.
	add []*nodeState
	// remove holds the records of unhealthy nodes.
	remove []removal
	// keptHealthy counts healthy nodes that already have a record here.
	keptHealthy int
	// unconfirmed holds healthy nodes missing here that are only healthy
	// because DNS listed them elsewhere at startup; they wait for a probe.
	unconfirmed []*nodeState
	view        recordView
}

type removal struct {
	record dnsRecord
	node   *nodeState
}

// matchRecords splits the records returned for name into those owned by a
// configured node and the IPs of every other A record. Records of another
// type or name are dropped: the API is asked for A records by exact name,
// and this filter makes sure nothing else is ever a candidate for deletion.
func matchRecords(name string, records []dnsRecord, nodes []*nodeState) (map[*nodeState][]dnsRecord, []string) {
	byIP := make(map[string]*nodeState, len(nodes))
	for _, s := range nodes {
		byIP[s.IP] = s
	}
	owned := map[*nodeState][]dnsRecord{}
	var other []string
	for _, rec := range records {
		if !strings.EqualFold(rec.Type, "A") || normalizeHostname(rec.Name) != name {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(rec.Content))
		var node *nodeState
		if err == nil && addr.Is4() {
			node = byIP[addr.String()]
		}
		if node == nil {
			other = append(other, rec.Content)
			continue
		}
		owned[node] = append(owned[node], rec)
	}
	slices.Sort(other)
	return owned, other
}

// planDomain compares the records under name with node health. The desired
// set is the healthy nodes; only records owned by a node are ever removed.
func planDomain(name string, records []dnsRecord, nodes []*nodeState) domainPlan {
	owned, other := matchRecords(name, records, nodes)
	plan := domainPlan{name: name, view: recordView{Other: other}}
	for _, s := range nodes {
		recs := owned[s]
		switch {
		case len(recs) > 0 && s.healthy:
			plan.keptHealthy++
			plan.view.Nodes = append(plan.view.Nodes, s.IP)
		case len(recs) > 0:
			for _, rec := range recs {
				plan.remove = append(plan.remove, removal{record: rec, node: s})
			}
			plan.view.Nodes = append(plan.view.Nodes, s.IP)
		case s.healthy && s.confirmed:
			plan.add = append(plan.add, s)
		case s.healthy:
			plan.unconfirmed = append(plan.unconfirmed, s)
		}
	}
	return plan
}

// reconcile brings every record in line with node health. A failure on one
// hostname does not stop the others, and the next round retries all of them.
func (w *Watcher) reconcile(ctx context.Context, now time.Time) {
	var failures []string
	var changes []DNSChange
	for _, name := range w.cfg.Records {
		records, err := w.dns.listARecords(ctx, name)
		if err != nil {
			w.log.Error("cannot list DNS records", "domain", name, "error", err)
			failures = append(failures, err.Error())
			continue
		}
		applied, err := w.applyPlan(ctx, planDomain(name, records, w.nodes), now)
		changes = append(changes, applied...)
		if err != nil {
			failures = append(failures, err.Error())
		}
	}

	w.counters.dnsChanges += len(changes)
	if len(changes) > 0 {
		alert := w.newAlert("dns_updated", describeChanges(changes))
		alert.Changes = changes
		w.alerts.send(alert)
	}
	if len(failures) > 0 {
		w.counters.reconcileErrors++
		w.lastReconcile = ReconcileStatus{At: now, Error: strings.Join(failures, "; ")}
		if !w.reconcileFailing {
			w.reconcileFailing = true
			alert := w.newAlert("dns_reconcile_failed", "DNS reconciliation failed; retrying every round")
			alert.Error = w.lastReconcile.Error
			w.alerts.send(alert)
		}
		return
	}
	w.lastReconcile = ReconcileStatus{At: now, OK: true}
	if w.reconcileFailing {
		w.reconcileFailing = false
		w.log.Info("DNS reconciliation succeeded again")
		w.alerts.send(w.newAlert("dns_reconcile_recovered", "DNS reconciliation succeeded again"))
	}
}

// applyPlan carries out one hostname's plan: additions first, then removals,
// and removals only once nothing they depend on is missing.
func (w *Watcher) applyPlan(ctx context.Context, plan domainPlan, now time.Time) ([]DNSChange, error) {
	view := recordView{Nodes: slices.Clone(plan.view.Nodes), Other: plan.view.Other}
	defer func() { w.records[plan.name] = view }()

	if len(plan.add) == 0 && len(plan.remove) == 0 {
		delete(w.dryRunPlans, plan.name)
		return nil, nil
	}
	if w.cfg.DryRun {
		w.logDryRun(plan, now)
		return nil, nil
	}

	var changes []DNSChange
	var failed []string
	added := 0
	// Additions go first so a replacement is live before anything is pulled.
	for _, s := range plan.add {
		rec, err := w.dns.createARecord(ctx, plan.name, s.IP, w.cfg.TTL)
		if err != nil {
			existing, found := w.findPublished(ctx, plan.name, s)
			if !found {
				w.log.Error("failed to add DNS record", "domain", plan.name, "node", s.Name, "ip", s.IP, "error", err)
				failed = append(failed, err.Error())
				continue
			}
			// A create whose reply was lost may still have stored the record.
			w.log.Info("create reported an error but the record is published", "domain", plan.name, "node", s.Name, "ip", s.IP, "error", err)
			rec = existing
		}
		added++
		view.Nodes = append(view.Nodes, s.IP)
		change := DNSChange{Action: "add", Domain: plan.name, Node: s.Name, IP: s.IP, RecordID: rec.ID,
			Reason: addReason(s, now), ConsecutiveFailures: s.failures}
		w.log.Info("dns record added", "domain", plan.name, "node", s.Name, "ip", s.IP, "record_id", rec.ID,
			"ttl", w.cfg.TTL, "reason", change.Reason, "consecutive_failures", s.failures, "consecutive_successes", s.successes)
		changes = append(changes, change)
	}

	if len(plan.remove) == 0 {
		return changes, joinFailures(plan.name, failed)
	}
	if len(failed) > 0 {
		w.log.Warn("DNS removals postponed until the additions succeed", "domain", plan.name, "pending_removals", removalIPs(plan.remove))
		return changes, joinFailures(plan.name, failed)
	}
	if plan.keptHealthy+added == 0 {
		// Every healthy node is still unconfirmed for this name. Removing now
		// would leave it without a single healthy record, so wait.
		w.log.Warn("refusing to remove the last DNS records of this domain until a healthy node is published here",
			"domain", plan.name, "pending_removals", removalIPs(plan.remove))
		return changes, nil
	}
	for _, rm := range plan.remove {
		if err := w.dns.deleteRecord(ctx, rm.record.ID); err != nil {
			w.log.Error("failed to remove DNS record", "domain", plan.name, "node", rm.node.Name, "ip", rm.node.IP,
				"record_id", rm.record.ID, "error", err)
			failed = append(failed, err.Error())
			continue
		}
		view.Nodes = removeFirst(view.Nodes, rm.node.IP)
		change := DNSChange{Action: "remove", Domain: plan.name, Node: rm.node.Name, IP: rm.node.IP, RecordID: rm.record.ID,
			Reason: removeReason(rm.node, w.limits.recover), ConsecutiveFailures: rm.node.failures}
		w.log.Warn("dns record removed", "domain", plan.name, "node", rm.node.Name, "ip", rm.node.IP, "record_id", rm.record.ID,
			"reason", change.Reason, "consecutive_failures", rm.node.failures)
		changes = append(changes, change)
	}
	return changes, joinFailures(plan.name, failed)
}

// findPublished re-reads name after a failed create and returns the node's
// record if it exists after all.
func (w *Watcher) findPublished(ctx context.Context, name string, s *nodeState) (dnsRecord, bool) {
	records, err := w.dns.listARecords(ctx, name)
	if err != nil {
		return dnsRecord{}, false
	}
	owned, _ := matchRecords(name, records, []*nodeState{s})
	if len(owned[s]) == 0 {
		return dnsRecord{}, false
	}
	return owned[s][0], true
}

// logDryRun reports what would change, once per distinct plan.
func (w *Watcher) logDryRun(plan domainPlan, now time.Time) {
	var adds, removes []string
	for _, s := range plan.add {
		adds = append(adds, s.Name+"="+s.IP)
	}
	for _, rm := range plan.remove {
		removes = append(removes, rm.node.Name+"="+rm.node.IP)
	}
	fingerprint := strings.Join(adds, ",") + "|" + strings.Join(removes, ",")
	if w.dryRunPlans[plan.name] == fingerprint {
		return
	}
	w.dryRunPlans[plan.name] = fingerprint
	for _, s := range plan.add {
		w.log.Info("dry-run: would add DNS record", "domain", plan.name, "node", s.Name, "ip", s.IP,
			"reason", addReason(s, now), "consecutive_failures", s.failures)
	}
	for _, rm := range plan.remove {
		w.log.Info("dry-run: would remove DNS record", "domain", plan.name, "node", rm.node.Name, "ip", rm.node.IP,
			"record_id", rm.record.ID, "reason", removeReason(rm.node, w.limits.recover), "consecutive_failures", rm.node.failures)
	}
}

func addReason(s *nodeState, now time.Time) string {
	if s.lastChange.Equal(now) {
		return fmt.Sprintf("node recovered after %d consecutive successful probes", s.successes)
	}
	return "node is healthy but has no record here"
}

func removeReason(s *nodeState, recover int) string {
	if s.successes > 0 || s.lastFailure == "" {
		return fmt.Sprintf("node not healthy yet (%d/%d consecutive successful probes)", s.successes, recover)
	}
	return "node unhealthy: " + s.lastFailure
}

func describeChanges(changes []DNSChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		verb := "added"
		if c.Action == "remove" {
			verb = "removed"
		}
		parts = append(parts, fmt.Sprintf("%s: %s %s (%s)", c.Domain, verb, c.Node, c.IP))
	}
	return strings.Join(parts, "; ")
}

func removalIPs(removals []removal) string {
	ips := make([]string, 0, len(removals))
	for _, rm := range removals {
		ips = append(ips, rm.node.IP)
	}
	return strings.Join(ips, ",")
}

func removeFirst(values []string, target string) []string {
	if i := slices.Index(values, target); i >= 0 {
		return slices.Delete(slices.Clone(values), i, i+1)
	}
	return values
}

func joinFailures(name string, failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return errors.New(name + ": " + strings.Join(failures, "; "))
}
