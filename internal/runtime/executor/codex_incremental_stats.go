package executor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// codexIncrementalRequestLogKey carries a request's codex_incremental record to
// the request log (usage_request_details.go).
const codexIncrementalRequestLogKey = "cliproxy.request_log.codex_incremental"

// codexIncrementalSummaryEvery is how often the summary line is logged.
const codexIncrementalSummaryEvery = 10 * time.Minute

// codexIncrementalRecord is what one request did, as the request log keeps it.
// It holds counts and reasons only, never content.
type codexIncrementalRecord struct {
	mu sync.Mutex
	// Mode is shadow or on, as it applied to this request.
	Mode string `json:"mode"`
	// Result is hit (the turn extends its chain), miss (it starts or restarts
	// one) or skip (it cannot take part).
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	// Transport is how the turn went upstream: http, or websocket. In shadow
	// mode it is always http.
	Transport  string `json:"transport"`
	InputItems int    `json:"input_items,omitempty"`
	NewItems   int    `json:"new_items,omitempty"`
	Echoed     int    `json:"echoed_items,omitempty"`
	// PrevItems and MismatchAt locate an input_prefix_mismatch: the previous
	// turn's item count and the first item this one changed.
	PrevItems  int `json:"prev_items,omitempty"`
	MismatchAt int `json:"mismatch_at,omitempty"`
	FullBytes  int `json:"full_bytes,omitempty"`
	// SentBytes is what went upstream, or in shadow mode what would have.
	SentBytes  int    `json:"sent_bytes,omitempty"`
	ChainTurn  int    `json:"chain_turn,omitempty"`
	ConnReused bool   `json:"conn_reused,omitempty"`
	ConnSerial uint64 `json:"conn,omitempty"`
	// Retried names why a turn sent as an increment went again in full.
	Retried string `json:"retried,omitempty"`
	// Fallback names why an "on" turn went over HTTP instead.
	Fallback string `json:"fallback,omitempty"`
}

func (r *codexIncrementalRecord) update(change func(*codexIncrementalRecord)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	change(r)
}

// snapshot copies the record for the request log, which may read it while the
// turn is still settling its transport.
func (r *codexIncrementalRecord) snapshot() map[string]any {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{"mode": r.Mode, "result": r.Result, "transport": r.Transport}
	setNonZero := func(key string, value any) {
		switch v := value.(type) {
		case string:
			if v != "" {
				out[key] = v
			}
		case int:
			if v != 0 {
				out[key] = v
			}
		case uint64:
			if v != 0 {
				out[key] = v
			}
		case bool:
			if v {
				out[key] = v
			}
		}
	}
	setNonZero("reason", r.Reason)
	setNonZero("input_items", r.InputItems)
	setNonZero("new_items", r.NewItems)
	setNonZero("echoed_items", r.Echoed)
	setNonZero("prev_items", r.PrevItems)
	if r.Reason == "input_prefix_mismatch" {
		// 0 is a real position here: the very first item changed.
		out["mismatch_at"] = r.MismatchAt
	}
	setNonZero("full_bytes", r.FullBytes)
	setNonZero("sent_bytes", r.SentBytes)
	setNonZero("chain_turn", r.ChainTurn)
	setNonZero("conn_reused", r.ConnReused)
	setNonZero("conn", r.ConnSerial)
	setNonZero("retried", r.Retried)
	setNonZero("fallback", r.Fallback)
	return out
}

func attachCodexIncrementalRecord(ctx context.Context, record *codexIncrementalRecord) {
	if ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil {
		ginCtx.Set(codexIncrementalRequestLogKey, record)
	}
}

func codexIncrementalRecordFromContext(ginCtx *gin.Context) map[string]any {
	if ginCtx == nil {
		return nil
	}
	value, exists := ginCtx.Get(codexIncrementalRequestLogKey)
	if !exists {
		return nil
	}
	record, ok := value.(*codexIncrementalRecord)
	if !ok {
		return nil
	}
	return record.snapshot()
}

// codexIncrementalStats aggregates records into the periodic summary line, the
// cheap way to read the hit rate off the journal.
type codexIncrementalStats struct {
	mu     sync.Mutex
	since  time.Time
	byMode map[string]*codexIncrementalCounters
}

type codexIncrementalCounters struct {
	turns, hits, misses, skips int64
	fullBytes, sentBytes       int64
	reasons                    map[string]int64
	websocket, fallback        int64
}

func newCodexIncrementalStats() *codexIncrementalStats {
	return &codexIncrementalStats{since: time.Now(), byMode: make(map[string]*codexIncrementalCounters)}
}

func (s *codexIncrementalStats) add(record *codexIncrementalRecord) {
	if s == nil || record == nil {
		return
	}
	record.mu.Lock()
	rec := struct {
		mode, result, reason, transport, fallback string
		full, sent                                int
	}{record.Mode, record.Result, record.Reason, record.Transport, record.Fallback, record.FullBytes, record.SentBytes}
	record.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	counters := s.byMode[rec.mode]
	if counters == nil {
		counters = &codexIncrementalCounters{reasons: make(map[string]int64)}
		s.byMode[rec.mode] = counters
	}
	counters.turns++
	switch rec.result {
	case "hit":
		counters.hits++
	case "miss":
		counters.misses++
	default:
		counters.skips++
	}
	if rec.reason != "" {
		counters.reasons[rec.reason]++
	}
	if rec.fallback != "" {
		counters.fallback++
		counters.reasons["fallback:"+rec.fallback]++
	}
	if rec.transport == "websocket" {
		counters.websocket++
	}
	counters.fullBytes += int64(rec.full)
	counters.sentBytes += int64(rec.sent)
}

// flush returns the summary lines since the last flush and starts a new period.
func (s *codexIncrementalStats) flush(now time.Time, size codexIncrementalPoolSize) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	modes := make([]string, 0, len(s.byMode))
	for mode := range s.byMode {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	lines := make([]string, 0, len(modes))
	for _, mode := range modes {
		c := s.byMode[mode]
		if c.turns == 0 {
			continue
		}
		saved := 0.0
		if c.fullBytes > 0 {
			saved = 100 * float64(c.fullBytes-c.sentBytes) / float64(c.fullBytes)
		}
		reasons := make([]string, 0, len(c.reasons))
		for reason, n := range c.reasons {
			reasons = append(reasons, fmt.Sprintf("%s=%d", reason, n))
		}
		sort.Strings(reasons)
		lines = append(lines, fmt.Sprintf("codex incremental summary mode=%s period=%s turns=%d hit=%d miss=%d skip=%d websocket=%d fallback=%d full_bytes=%d sent_bytes=%d saved=%.1f%% chains=%d conns=%d reasons={%s}",
			mode, now.Sub(s.since).Round(time.Second), c.turns, c.hits, c.misses, c.skips, c.websocket, c.fallback,
			c.fullBytes, c.sentBytes, saved, size.chains, size.conns, strings.Join(reasons, ",")))
	}
	s.byMode = make(map[string]*codexIncrementalCounters)
	s.since = now
	return lines
}

var (
	codexIncrementalSharedOnce sync.Once
	codexIncrementalShared     atomic.Pointer[codexIncrementalPool]
)

// sharedCodexIncrementalPool is the process-wide pool. It outlives executors,
// which a config reload replaces, so a reload does not cut every chain. It is
// created by the first request that takes part, so a process with the feature
// off never starts its timer.
func sharedCodexIncrementalPool() *codexIncrementalPool {
	codexIncrementalSharedOnce.Do(func() {
		pool := newCodexIncrementalPool()
		codexIncrementalShared.Store(pool)
		go pool.maintain(codexIncrementalSummaryEvery)
	})
	return codexIncrementalShared.Load()
}

// maintain frees idle connections every half minute and logs the summary.
func (p *codexIncrementalPool) maintain(summaryEvery time.Duration) {
	sweep := time.NewTicker(30 * time.Second)
	defer sweep.Stop()
	summary := time.NewTicker(summaryEvery)
	defer summary.Stop()
	for {
		select {
		case <-sweep.C:
			p.sweep(p.lastLimits())
		case now := <-summary.C:
			for _, line := range p.stats.flush(now, p.size()) {
				log.Info(line)
			}
		}
	}
}
