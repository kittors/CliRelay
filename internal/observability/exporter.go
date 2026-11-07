// Package observability exports the process's runtime metrics in the
// Prometheus text format on /metrics.
//
// It is a pull endpoint on purpose. Prometheus scrapes it directly, and an
// OpenTelemetry Collector reads the same page through its prometheus receiver
// and forwards it as OTLP, so one exposition serves both without the process
// carrying an exporter, a push schedule and a retry queue of its own.
//
// Everything is process-local: in a cluster, or while a blue/green handover
// briefly runs two slots, each process reports its own counters and they start
// from zero when it starts, which rate() and increase() already expect.
package observability

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	metricNamespace = "clirelay"

	// overflowLabel replaces a label value once its family has admitted
	// maxModelSeries / maxAccountSeries distinct values. Model names come from
	// clients and some upstreams accept any name, so without a cap one caller
	// could grow the registry, and every scrape, without limit.
	overflowLabel = "__other__"
	unknownLabel  = "unknown"
	// maxLabelLength bounds a single label value; real provider, model and
	// route names are far shorter.
	maxLabelLength = 128

	maxModelSeries   = 1000
	maxAccountSeries = 5000
)

// Sources supplies the state read at scrape time. Both functions may be nil.
type Sources struct {
	// Auths lists every credential of every tenant.
	Auths func() []*coreauth.Auth
	// InFlight is the number of HTTP requests being served.
	InFlight func() int64
}

// Exporter owns the metric families, the scrape endpoint and its settings.
type Exporter struct {
	registry *prometheus.Registry

	httpRequests       *prometheus.CounterVec
	httpDuration       *prometheus.HistogramVec
	upstreamRequests   *prometheus.CounterVec
	upstreamDuration   *prometheus.HistogramVec
	upstreamFirstToken *prometheus.HistogramVec
	upstreamTokens     *prometheus.CounterVec
	accountRequests    *prometheus.CounterVec

	models   *labelGuard
	accounts *labelGuard

	sources atomic.Pointer[Sources]
	now     func() time.Time

	settings settingsState
}

var (
	defaultOnce     sync.Once
	defaultExporter *Exporter
)

// Default returns the process-wide exporter, creating it on first use.
func Default() *Exporter {
	defaultOnce.Do(func() {
		defaultExporter = NewExporter()
	})
	return defaultExporter
}

// NewExporter builds an exporter with its own registry. Tests use it to keep
// their observations apart from the process-wide one.
func NewExporter() *Exporter {
	e := &Exporter{
		registry: prometheus.NewRegistry(),
		models:   newLabelGuard(maxModelSeries),
		accounts: newLabelGuard(maxAccountSeries),
		now:      time.Now,
	}
	e.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "http_requests_total",
		Help:      "HTTP requests served, by route template and the status code sent to the client.",
	}, []string{"method", "route", "status_code"})
	e.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "http_request_duration_seconds",
		Help:      "Time to serve an HTTP request, including the whole of a streamed response.",
		Buckets:   []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"method", "route"})
	e.upstreamRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "upstream_requests_total",
		Help:      "Requests sent to an upstream provider; every attempt of a retried request counts.",
	}, []string{"provider", "model", "outcome"})
	e.upstreamDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "upstream_request_duration_seconds",
		Help:      "Duration of successful upstream requests, from dispatch to the end of the response.",
		Buckets:   []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
	}, []string{"provider", "model"})
	e.upstreamFirstToken = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "upstream_first_token_seconds",
		Help:      "Time from dispatch to the first byte sent to the client, for successful streamed requests.",
		Buckets:   []float64{0.25, 0.5, 1, 2, 3, 5, 8, 13, 20, 30, 60},
	}, []string{"provider", "model"})
	e.upstreamTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "upstream_tokens_total",
		Help:      "Tokens reported by upstreams. type=input is the uncached part of the prompt; cache_read and cache_write are the rest of it; reasoning is the part of output the upstream reported as reasoning.",
	}, []string{"provider", "model", "type"})
	e.accountRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "upstream_account_requests_total",
		Help:      "Upstream requests per AI account. auth_index is the account's stable opaque index, the one request logs carry; account names and emails are never exported.",
	}, []string{"provider", "auth_index", "outcome"})

	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "build_info",
		Help:      "Always 1; the labels describe the running build.",
		ConstLabels: prometheus.Labels{
			"version":    buildinfo.Version,
			"commit":     buildinfo.Commit,
			"build_date": buildinfo.BuildDate,
		},
	})
	buildInfo.Set(1)

	e.registry.MustRegister(
		e.httpRequests, e.httpDuration,
		e.upstreamRequests, e.upstreamDuration, e.upstreamFirstToken, e.upstreamTokens,
		e.accountRequests,
		buildInfo,
		newStateCollector(e),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return e
}

// Registry exposes the underlying registry, for tests that gather directly.
func (e *Exporter) Registry() *prometheus.Registry {
	if e == nil {
		return nil
	}
	return e.registry
}

// SetSources replaces the scrape-time state sources.
func (e *Exporter) SetSources(sources Sources) {
	if e == nil {
		return
	}
	e.sources.Store(&sources)
}

func (e *Exporter) currentSources() Sources {
	if e == nil {
		return Sources{}
	}
	if s := e.sources.Load(); s != nil {
		return *s
	}
	return Sources{}
}

// ObserveHTTP records one served HTTP request. route is the router's template
// (gin's FullPath), empty when no route matched.
func (e *Exporter) ObserveHTTP(method, route string, status int, elapsed time.Duration) {
	if e == nil {
		return
	}
	method = normalizeMethod(method)
	route = labelValue(route, "unmatched")
	e.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	e.httpDuration.WithLabelValues(method, route).Observe(elapsed.Seconds())
}

// knownMethods keeps the method label to the standard verbs; anything else a
// client invents is reported as OTHER rather than becoming a series.
var knownMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodOptions: {},
}

func normalizeMethod(method string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if _, ok := knownMethods[method]; ok {
		return method
	}
	return "OTHER"
}

// labelValue trims value, replaces invalid UTF-8 (the client library rejects
// it) and caps its length; an empty value becomes fallback.
func labelValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	if len(value) > maxLabelLength {
		cut := maxLabelLength
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = value[:cut]
	}
	return value
}

// labelGuard admits at most limit distinct values; later new values are
// reported as overflowLabel. Admitted values stay admitted for the life of the
// process, matching the series the registry already holds for them.
type labelGuard struct {
	mu     sync.Mutex
	limit  int
	values map[string]struct{}
}

func newLabelGuard(limit int) *labelGuard {
	return &labelGuard{limit: limit, values: make(map[string]struct{})}
}

func (g *labelGuard) admit(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.values[key]; ok {
		return true
	}
	if len(g.values) >= g.limit {
		return false
	}
	g.values[key] = struct{}{}
	return true
}
