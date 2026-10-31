package diagnostics

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// Each request diagnostic carries the request's route trace
// (sdk/cliproxy/routetrace): the scheduler records into it through the
// request context, Snapshot writes it into the request log, and the in-flight
// view below reads its live stage.

// inflightShards spreads the in-flight set over independent locks: every
// gateway request enters and leaves it once.
const inflightShards = 32

// MaxInFlightListed bounds one listing. The oldest requests come first, which
// are the ones worth looking at.
const MaxInFlightListed = 500

type inflightShard struct {
	mu       sync.Mutex
	requests map[*RequestDiagnostic]struct{}
}

var (
	inflight    [inflightShards]inflightShard
	inflightSeq atomic.Uint64
)

func newRequestDiagnostic() *RequestDiagnostic {
	return &RequestDiagnostic{
		trace:       routetrace.New(),
		startedAt:   time.Now(),
		inflightSeq: inflightSeq.Add(1),
	}
}

// SetTenant records the tenant the request was authenticated for; the
// in-flight view shows a request to that tenant only.
func SetTenant(c *gin.Context, tenantID string) {
	d := FromGin(c)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tenantID = strings.TrimSpace(tenantID)
}

// BeginInFlight lists the request among those in flight and returns the
// function that ends it: the request leaves the list and its route trace is
// closed, which publishes any usage record still held for it (see
// routetrace.Recorder.Close). The request logging middleware calls it when it
// creates the diagnostic and defers the end, so it runs however the request
// ends, a panic included. A diagnostic already in flight gets a no-op: only
// its first owner ends it.
func BeginInFlight(d *RequestDiagnostic) (end func()) {
	if d == nil {
		return func() {}
	}
	shard := &inflight[d.inflightSeq%inflightShards]
	shard.mu.Lock()
	if _, listed := shard.requests[d]; listed {
		shard.mu.Unlock()
		return func() {}
	}
	if shard.requests == nil {
		shard.requests = make(map[*RequestDiagnostic]struct{})
	}
	shard.requests[d] = struct{}{}
	shard.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			shard.mu.Lock()
			delete(shard.requests, d)
			shard.mu.Unlock()
			d.trace.Close()
		})
	}
}

// InFlightRequest is one request being served, as the in-flight view lists it.
// It names the key by id and name only, never by its secret.
type InFlightRequest struct {
	RequestID      string           `json:"request_id"`
	Method         string           `json:"method,omitempty"`
	Path           string           `json:"path,omitempty"`
	StartedAt      time.Time        `json:"started_at"`
	ElapsedMs      int64            `json:"elapsed_ms"`
	Stage          string           `json:"stage"`
	StageElapsedMs int64            `json:"stage_elapsed_ms"`
	Model          string           `json:"model,omitempty"`
	Stream         bool             `json:"stream,omitempty"`
	Attempt        int              `json:"attempt,omitempty"`
	Account        *InFlightAccount `json:"account,omitempty"`
	APIKeyID       string           `json:"api_key_id,omitempty"`
	APIKeyName     string           `json:"api_key_name,omitempty"`
}

// InFlightAccount is the account a request is on, or last tried.
type InFlightAccount struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label,omitempty"`
	Provider string `json:"provider,omitempty"`
	Group    string `json:"group,omitempty"`
	Model    string `json:"model,omitempty"`
}

// InFlight lists the authenticated requests in flight whose tenant belongs
// accepts, oldest first, at most MaxInFlightListed of them, and how many there
// are in all. A WebSocket connection between two of its requests is not in
// flight.
func InFlight(belongs func(tenantID string) bool, now time.Time) ([]InFlightRequest, int) {
	var listed []*RequestDiagnostic
	for i := range inflight {
		shard := &inflight[i]
		shard.mu.Lock()
		for d := range shard.requests {
			listed = append(listed, d)
		}
		shard.mu.Unlock()
	}
	out := make([]InFlightRequest, 0, len(listed))
	for _, d := range listed {
		entry, ok := d.inFlightEntry(belongs, now)
		if ok {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].RequestID < out[j].RequestID
	})
	total := len(out)
	if total > MaxInFlightListed {
		out = out[:MaxInFlightListed]
	}
	return out, total
}

func (d *RequestDiagnostic) inFlightEntry(belongs func(string) bool, now time.Time) (InFlightRequest, bool) {
	d.mu.Lock()
	if !d.authenticated || (belongs != nil && !belongs(d.tenantID)) {
		d.mu.Unlock()
		return InFlightRequest{}, false
	}
	entry := InFlightRequest{RequestID: d.RequestID, Method: d.Method, Path: d.OriginalPath}
	if d.Auth != nil {
		entry.APIKeyID = d.Auth.APIKeyID
		entry.APIKeyName = d.Auth.APIKeyName
	}
	d.mu.Unlock()

	live := d.trace.Live()
	if live.Stage == routetrace.StageIdle {
		return InFlightRequest{}, false
	}
	started := live.StartedAt
	if started.IsZero() {
		started = d.startedAt
	}
	entry.StartedAt = started.Truncate(time.Millisecond)
	entry.ElapsedMs = max(now.Sub(started).Milliseconds(), 0)
	entry.Stage = live.Stage
	entry.StageElapsedMs = max(now.Sub(live.StageSince).Milliseconds(), 0)
	entry.Model = live.Model
	entry.Stream = live.Stream
	entry.Attempt = live.Attempt
	if live.AuthID != "" {
		entry.Account = &InFlightAccount{
			AuthID:   live.AuthID,
			Label:    live.AuthLabel,
			Provider: live.Provider,
			Group:    live.Group,
			Model:    live.UpstreamModel,
		}
	}
	return entry, true
}
