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
	// ProxyMismatch are the IPs of nodes with a record here whose proxy
	// status is not the configured one.
	ProxyMismatch []string
}

// DNSChange is one record dnswatch added, removed or proxied.
type DNSChange struct {
	// Action is add, remove, or proxy for a record whose proxy was turned on.
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
	// remove holds the records of unhealthy and sidelined nodes.
	remove []nodeRecord
	// proxy holds the DNS-only records of kept nodes; proxied mode turns the
	// proxy on for them before anything here is removed.
	proxy []nodeRecord
	// keptHealthy counts healthy nodes that already have a record here.
	keptHealthy int
	// keptProxied counts the proxied records that stay whatever the plan
	// does: the kept nodes' and those outside the node list.
	keptProxied int
	// proxiedNow says some A record here is proxied. Cloudflare then serves
	// all of them as proxied, and serves the rest DNS only, with their
	// addresses public, once the last proxied one is gone.
	proxiedNow bool
	// mismatched holds the node records whose proxy status is not the
	// configured one.
	mismatched []nodeRecord
	// unconfirmed holds healthy nodes missing here that are only healthy
	// because DNS listed them elsewhere at startup; they wait for a probe.
	unconfirmed []*nodeState
	view        recordView
}

// nodeRecord is a published record and the configured node it points to.
type nodeRecord struct {
	record dnsRecord
	node   *nodeState
}

// matchRecords splits the records returned for name into those owned by a
// configured node and the IPs of every other A record, and counts how many of
// the latter are proxied. Records of another type or name are dropped: the
// API is asked for A records by exact name, and this filter makes sure
// nothing else is ever a candidate for deletion or for a proxy change.
func matchRecords(name string, records []dnsRecord, nodes []*nodeState) (owned map[*nodeState][]dnsRecord, other []string, otherProxied int) {
	byIP := make(map[string]*nodeState, len(nodes))
	for _, s := range nodes {
		byIP[s.IP] = s
	}
	owned = map[*nodeState][]dnsRecord{}
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
			if rec.Proxied {
				otherProxied++
			}
			continue
		}
		owned[node] = append(owned[node], rec)
	}
	slices.Sort(other)
	return owned, other, otherProxied
}

// mismatchedRecords returns the node records, in node order, whose proxy
// status differs from proxied.
func mismatchedRecords(owned map[*nodeState][]dnsRecord, nodes []*nodeState, proxied bool) []nodeRecord {
	var out []nodeRecord
	for _, s := range nodes {
		for _, rec := range owned[s] {
			if rec.Proxied != proxied {
				out = append(out, nodeRecord{record: rec, node: s})
			}
		}
	}
	return out
}

// planDomain compares the records under name with node health. The desired
// set is the healthy nodes, which with the egress probe on means the ready
// nodes markDesired did not sideline; only records owned by a node are ever
// removed, and with proxied set only those have their proxy turned on.
func planDomain(name string, records []dnsRecord, nodes []*nodeState, proxied bool) domainPlan {
	owned, other, otherProxied := matchRecords(name, records, nodes)
	plan := domainPlan{
		name:        name,
		view:        recordView{Other: other},
		keptProxied: otherProxied,
		proxiedNow:  otherProxied > 0,
		mismatched:  mismatchedRecords(owned, nodes, proxied),
	}
	for _, s := range nodes {
		recs := owned[s]
		wanted := s.healthy && !s.sidelined
		for _, rec := range recs {
			plan.proxiedNow = plan.proxiedNow || rec.Proxied
		}
		switch {
		case len(recs) > 0 && wanted:
			plan.keptHealthy++
			plan.view.Nodes = append(plan.view.Nodes, s.IP)
			for _, rec := range recs {
				switch {
				case rec.Proxied:
					plan.keptProxied++
				case proxied:
					plan.proxy = append(plan.proxy, nodeRecord{record: rec, node: s})
				}
			}
		case len(recs) > 0:
			for _, rec := range recs {
				plan.remove = append(plan.remove, nodeRecord{record: rec, node: s})
			}
			plan.view.Nodes = append(plan.view.Nodes, s.IP)
		case wanted && s.confirmed && !s.egressPending:
			plan.add = append(plan.add, s)
		case wanted:
			plan.unconfirmed = append(plan.unconfirmed, s)
		}
	}
	plan.view.ProxyMismatch = recordIPs(plan.mismatched, nil)
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
		applied, err := w.applyPlan(ctx, planDomain(name, records, w.nodes, w.cfg.Proxied), now)
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

// applyPlan carries out one hostname's plan: additions first, then turning
// the proxy on, then removals, and removals only once nothing they depend on
// is missing.
func (w *Watcher) applyPlan(ctx context.Context, plan domainPlan, now time.Time) ([]DNSChange, error) {
	view := recordView{Nodes: slices.Clone(plan.view.Nodes), Other: plan.view.Other}
	// settled holds the mismatched records proxied or removed in this round.
	settled := map[string]bool{}
	defer func() {
		view.ProxyMismatch = recordIPs(plan.mismatched, settled)
		w.records[plan.name] = view
	}()
	w.noteUnmanagedProxies(plan)

	if len(plan.add) == 0 && len(plan.remove) == 0 && len(plan.proxy) == 0 {
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
		rec, err := w.dns.createARecord(ctx, plan.name, s.IP, w.cfg.TTL, w.cfg.Proxied)
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
			"ttl", w.cfg.TTL, "proxied", w.cfg.Proxied, "reason", change.Reason,
			"consecutive_failures", s.failures, "consecutive_successes", s.successes)
		changes = append(changes, change)
	}
	additionsFailed := len(failed) > 0

	proxiedNow := 0
	for _, pr := range plan.proxy {
		if _, err := w.dns.proxyRecord(ctx, pr.record.ID); err != nil {
			w.log.Error("failed to turn on the proxy of a DNS record", "domain", plan.name, "node", pr.node.Name, "ip", pr.node.IP,
				"record_id", pr.record.ID, "error", err)
			failed = append(failed, err.Error())
			continue
		}
		proxiedNow++
		settled[pr.record.ID] = true
		change := DNSChange{Action: "proxy", Domain: plan.name, Node: pr.node.Name, IP: pr.node.IP, RecordID: pr.record.ID,
			Reason: "record was DNS only and proxied is set"}
		w.log.Info("dns record proxied", "domain", plan.name, "node", pr.node.Name, "ip", pr.node.IP, "record_id", pr.record.ID,
			"reason", change.Reason)
		changes = append(changes, change)
	}

	if len(plan.remove) == 0 {
		return changes, joinFailures(plan.name, failed)
	}
	pending := strings.Join(recordIPs(plan.remove, nil), ",")
	if additionsFailed {
		w.log.Warn("DNS removals postponed until the additions succeed", "domain", plan.name, "pending_removals", pending)
		return changes, joinFailures(plan.name, failed)
	}
	if plan.keptHealthy+added == 0 {
		// Every healthy node is still unconfirmed for this name. Removing now
		// would leave it without a single healthy record, so wait.
		w.log.Warn("refusing to remove the last DNS records of this domain until a healthy node is published here",
			"domain", plan.name, "pending_removals", pending)
		return changes, joinFailures(plan.name, failed)
	}
	stayProxied := plan.keptProxied + proxiedNow
	if w.cfg.Proxied {
		stayProxied += added
	}
	if plan.proxiedNow && stayProxied == 0 {
		// The records left would all be DNS only, and Cloudflare would start
		// handing out their nodes' addresses, which cannot be taken back. In
		// proxied mode this lasts until the proxy goes on for a kept record;
		// with proxied off it lasts until an operator settles the proxy status
		// of the domain (see noteUnmanagedProxies).
		w.log.Warn("DNS removals postponed: they would remove the last proxied record of this domain and leave it DNS only, publishing the addresses of the nodes left",
			"domain", plan.name, "pending_removals", pending, "proxied", w.cfg.Proxied)
		if len(failed) == 0 {
			failed = append(failed, "removal of "+pending+" withheld: it would remove the last proxied record")
		}
		return changes, joinFailures(plan.name, failed)
	}
	for _, rm := range plan.remove {
		if err := w.dns.deleteRecord(ctx, rm.record.ID); err != nil {
			w.log.Error("failed to remove DNS record", "domain", plan.name, "node", rm.node.Name, "ip", rm.node.IP,
				"record_id", rm.record.ID, "error", err)
			failed = append(failed, err.Error())
			continue
		}
		settled[rm.record.ID] = true
		view.Nodes = removeFirst(view.Nodes, rm.node.IP)
		change := DNSChange{Action: "remove", Domain: plan.name, Node: rm.node.Name, IP: rm.node.IP, RecordID: rm.record.ID,
			Reason: removeReason(rm.node, w.limits.recover), ConsecutiveFailures: consecutiveFailures(rm.node)}
		w.log.Warn("dns record removed", "domain", plan.name, "node", rm.node.Name, "ip", rm.node.IP, "record_id", rm.record.ID,
			"reason", change.Reason, "consecutive_failures", change.ConsecutiveFailures)
		changes = append(changes, change)
	}
	return changes, joinFailures(plan.name, failed)
}

// noteUnmanagedProxies warns, once per record, about node records that are
// proxied while proxied is off. dnswatch leaves them proxied: turning the
// proxy off would publish the node's address. While one of them exists,
// Cloudflare also serves the DNS-only records of the domain as proxied, so
// the config most likely should say proxied: true.
func (w *Watcher) noteUnmanagedProxies(plan domainPlan) {
	if w.cfg.Proxied {
		// The mismatched records are DNS-only ones, which applyPlan proxies.
		return
	}
	current := map[string]bool{}
	for _, nr := range plan.mismatched {
		current[nr.record.ID] = true
		if w.proxyNoted[plan.name][nr.record.ID] {
			continue
		}
		w.log.Warn("DNS record is proxied but proxied is off; dnswatch will not turn the proxy off, which would publish the node's address. Set proxied: true to manage this domain proxied",
			"domain", plan.name, "node", nr.node.Name, "ip", nr.node.IP, "record_id", nr.record.ID)
	}
	w.proxyNoted[plan.name] = current
}

// findPublished re-reads name after a failed create and returns the node's
// record if it exists after all.
func (w *Watcher) findPublished(ctx context.Context, name string, s *nodeState) (dnsRecord, bool) {
	records, err := w.dns.listARecords(ctx, name)
	if err != nil {
		return dnsRecord{}, false
	}
	owned, _, _ := matchRecords(name, records, []*nodeState{s})
	if len(owned[s]) == 0 {
		return dnsRecord{}, false
	}
	return owned[s][0], true
}

// logDryRun reports what would change, once per distinct plan.
func (w *Watcher) logDryRun(plan domainPlan, now time.Time) {
	var adds, removes, proxies []string
	for _, s := range plan.add {
		adds = append(adds, s.Name+"="+s.IP)
	}
	for _, rm := range plan.remove {
		removes = append(removes, rm.node.Name+"="+rm.node.IP)
	}
	for _, pr := range plan.proxy {
		proxies = append(proxies, pr.node.Name+"="+pr.node.IP+"#"+pr.record.ID)
	}
	fingerprint := strings.Join(adds, ",") + "|" + strings.Join(removes, ",") + "|" + strings.Join(proxies, ",")
	if w.dryRunPlans[plan.name] == fingerprint {
		return
	}
	w.dryRunPlans[plan.name] = fingerprint
	for _, s := range plan.add {
		w.log.Info("dry-run: would add DNS record", "domain", plan.name, "node", s.Name, "ip", s.IP,
			"proxied", w.cfg.Proxied, "reason", addReason(s, now), "consecutive_failures", s.failures)
	}
	for _, pr := range plan.proxy {
		w.log.Info("dry-run: would turn on the proxy of DNS record", "domain", plan.name, "node", pr.node.Name, "ip", pr.node.IP,
			"record_id", pr.record.ID)
	}
	for _, rm := range plan.remove {
		w.log.Info("dry-run: would remove DNS record", "domain", plan.name, "node", rm.node.Name, "ip", rm.node.IP,
			"record_id", rm.record.ID, "reason", removeReason(rm.node, w.limits.recover), "consecutive_failures", consecutiveFailures(rm.node))
	}
}

func addReason(s *nodeState, now time.Time) string {
	switch {
	case s.lastChange.Equal(now):
		return fmt.Sprintf("node recovered after %d consecutive successful probes", s.successes)
	case s.egress != nil && !s.egress.healthy:
		return "node is ready and no node passes its egress check"
	case s.egress != nil && s.egress.lastChange.Equal(now):
		return fmt.Sprintf("node egress recovered after %d consecutive successful egress probes", s.egress.successes)
	default:
		return "node is healthy but has no record here"
	}
}

func removeReason(s *nodeState, recover int) string {
	if s.sidelined {
		e := s.egress
		if e.successes > 0 || e.lastFailure == "" {
			return fmt.Sprintf("node degraded: egress check not passing yet (%d/%d consecutive successful egress probes)", e.successes, recover)
		}
		return "node degraded: egress check failed: " + e.lastFailure
	}
	if s.successes > 0 || s.lastFailure == "" {
		return fmt.Sprintf("node not healthy yet (%d/%d consecutive successful probes)", s.successes, recover)
	}
	return "node unhealthy: " + s.lastFailure
}

func describeChanges(changes []DNSChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		verb := "added"
		switch c.Action {
		case "remove":
			verb = "removed"
		case "proxy":
			verb = "proxied"
		}
		parts = append(parts, fmt.Sprintf("%s: %s %s (%s)", c.Domain, verb, c.Node, c.IP))
	}
	return strings.Join(parts, "; ")
}

// recordIPs returns the node IPs of records, each once and in order, leaving
// out the records whose id is in skip.
func recordIPs(records []nodeRecord, skip map[string]bool) []string {
	var ips []string
	for _, nr := range records {
		if !skip[nr.record.ID] && !slices.Contains(ips, nr.node.IP) {
			ips = append(ips, nr.node.IP)
		}
	}
	return ips
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
