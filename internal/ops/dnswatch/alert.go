package dnswatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	alertQueueSize    = 64
	alertPostTimeout  = 10 * time.Second
	alertDrainTimeout = 5 * time.Second
)

// Alert is the JSON body POSTed to alert_webhook.
//
// Events: node_unhealthy, node_healthy, dns_updated, no_healthy_nodes,
// no_healthy_nodes_resolved, dns_reconcile_failed, dns_reconcile_recovered,
// dns_hold_started, dns_hold_ended.
type Alert struct {
	Event string `json:"event"`
	// Text is a one-line summary; the name suits Slack-style incoming hooks.
	Text                 string      `json:"text"`
	Time                 time.Time   `json:"time"`
	Node                 string      `json:"node,omitempty"`
	IP                   string      `json:"ip,omitempty"`
	Reason               string      `json:"reason,omitempty"`
	ConsecutiveFailures  int         `json:"consecutive_failures,omitempty"`
	ConsecutiveSuccesses int         `json:"consecutive_successes,omitempty"`
	Changes              []DNSChange `json:"changes,omitempty"`
	Error                string      `json:"error,omitempty"`
	HealthyNodes         []string    `json:"healthy_nodes"`
	UnhealthyNodes       []string    `json:"unhealthy_nodes"`
	DryRun               bool        `json:"dry_run"`
	Hold                 bool        `json:"hold"`
}

// alerter posts alerts from a background goroutine so a slow or dead
// webhook never delays probing. A nil alerter drops everything.
type alerter struct {
	url    string
	client *http.Client
	log    *slog.Logger
	queue  chan Alert
	done   chan struct{}

	mu     sync.Mutex
	closed bool
}

func newAlerter(webhook string, client *http.Client, logger *slog.Logger) *alerter {
	if webhook == "" {
		return nil
	}
	a := &alerter{
		url:    webhook,
		client: client,
		log:    logger,
		queue:  make(chan Alert, alertQueueSize),
		done:   make(chan struct{}),
	}
	go a.loop()
	return a
}

func (a *alerter) send(alert Alert) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	select {
	case a.queue <- alert:
	default:
		a.log.Warn("alert queue full; dropping alert", "event", alert.Event)
	}
}

func (a *alerter) loop() {
	defer close(a.done)
	for alert := range a.queue {
		a.post(alert)
	}
}

func (a *alerter) post(alert Alert) {
	body, err := json.Marshal(alert)
	if err != nil {
		a.log.Error("cannot encode alert", "event", alert.Event, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), alertPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		a.log.Error("cannot build alert request", "event", alert.Event, "error", withoutURL(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Warn("alert webhook unreachable", "event", alert.Event, "error", withoutURL(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		a.log.Warn("alert webhook rejected the alert", "event", alert.Event, "status", resp.StatusCode)
	}
}

// close stops accepting alerts and waits up to timeout for queued ones.
func (a *alerter) close(timeout time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.queue)
	}
	a.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-a.done:
	case <-timer.C:
		a.log.Warn("shutting down with undelivered alerts")
	}
}

// withoutURL drops the URL from HTTP client errors: chat webhook URLs carry
// their secret in the path, so they must not reach the log.
func withoutURL(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}

// webhookOrigin is the part of the webhook URL that is safe to log.
func webhookOrigin(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	return u.Scheme + "://" + u.Host
}
