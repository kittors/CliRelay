package sharedstate

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
)

// Per-subject request and token windows.
//
// Each subject (an API key or an end user) has one counter per wall-clock
// minute for requests and one for tokens. The trailing-minute value is the
// current bucket plus the previous bucket weighted by the part of it that is
// still inside the last 60 seconds. That approximates the node-local sliding
// window closely without storing one entry per request, and unlike a plain
// per-minute counter it does not let a client spend two minutes of budget
// across a minute boundary.
const (
	windowMillis = int64(time.Minute / time.Millisecond)
	// bucketTTL keeps a bucket while it can still be the previous minute.
	bucketTTL = 3 * time.Minute
)

// admitScript counts one request for a subject, reads its token window and,
// when a concurrency limit applies, takes a slot: every quota input a request
// needs in one round trip.
//
// KEYS[1] requests, current minute   KEYS[2] requests, previous minute
// KEYS[3] tokens, current minute     KEYS[4] tokens, previous minute
// KEYS[5] concurrency slot set
// ARGV[1] now (ms)   ARGV[2] ms elapsed in the current minute   ARGV[3] bucket TTL (ms)
// ARGV[4] concurrency limit (0: take no slot)   ARGV[5] lease member
// ARGV[6] lease expiry (ms)   ARGV[7] slot set TTL (ms)
// Returns {requests x1000, tokens x1000, slot taken (0/1), slots held}.
var admitScript = redis.NewScript(`
local rpmCur = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
local weight = (60000 - tonumber(ARGV[2])) / 60000
local rpmPrev = tonumber(redis.call('GET', KEYS[2]) or '0')
local tpmCur = tonumber(redis.call('GET', KEYS[3]) or '0')
local tpmPrev = tonumber(redis.call('GET', KEYS[4]) or '0')
local rpm = rpmPrev * weight + rpmCur
local tpm = tpmPrev * weight + tpmCur
local taken = 0
local held = 0
local limit = tonumber(ARGV[4])
if limit > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[5], '-inf', ARGV[1])
  held = redis.call('ZCARD', KEYS[5])
  if held < limit then
    redis.call('ZADD', KEYS[5], ARGV[6], ARGV[5])
    redis.call('PEXPIRE', KEYS[5], ARGV[7])
    taken = 1
    held = held + 1
  end
end
return {math.floor(rpm * 1000), math.floor(tpm * 1000), taken, held}
`)

// Admission is the cluster-wide view of one admitted request.
type Admission struct {
	// RPM and TPM are the subject's trailing-minute request and token counts
	// across the cluster, including this request.
	RPM float64
	TPM float64
	// SlotTaken reports whether a concurrency slot is now held for the
	// request. Held is the number of slots in use, including this one when
	// taken.
	SlotTaken bool
	Held      int
	// Release gives the slot back. It is safe to call more than once and is a
	// no-op when no slot was taken.
	Release func()
}

// Rate is one subject's cluster-wide trailing-minute usage.
type Rate struct {
	RPM float64
	TPM float64
}

func subjectKeys(subject string, minute int64) (rpmCur, rpmPrev, tpmCur, tpmPrev, slots string) {
	// The braces are a Redis Cluster hash tag: all of a subject's keys land on
	// one shard, which multi-key scripts require. Harmless on a single node.
	base := sharedredis.KeyPrefix + "rl:{" + sharedredis.HashPart(subject) + "}:"
	cur := strconv.FormatInt(minute, 10)
	prev := strconv.FormatInt(minute-1, 10)
	return base + "rpm:" + cur, base + "rpm:" + prev, base + "tpm:" + cur, base + "tpm:" + prev, base + "conc"
}

func minuteOf(now time.Time) (minute, elapsed int64) {
	ms := now.UnixMilli()
	return ms / windowMillis, ms % windowMillis
}

// AdmitRequest counts one request for subject and, when concurrencyLimit is
// positive, tries to take one of its concurrency slots, all atomically. The
// caller decides admission from the returned view and must call Release when
// the request ends, whether or not it was admitted.
func (s *Store) AdmitRequest(ctx context.Context, subject string, concurrencyLimit int) (Admission, error) {
	if concurrencyLimit < 0 {
		concurrencyLimit = 0
	}
	now := s.now()
	minute, elapsed := minuteOf(now)
	rpmCur, rpmPrev, tpmCur, tpmPrev, slots := subjectKeys(subject, minute)
	member := ""
	if concurrencyLimit > 0 {
		member = s.newLeaseMember()
	}
	res, err := s.client.Eval(ctx, admitScript,
		[]string{rpmCur, rpmPrev, tpmCur, tpmPrev, slots},
		now.UnixMilli(), elapsed, bucketTTL.Milliseconds(),
		concurrencyLimit, member, now.Add(s.leaseTTL).UnixMilli(), s.setTTL().Milliseconds())
	if err != nil {
		return Admission{}, err
	}
	vals, err := int64s(res, 4)
	if err != nil {
		return Admission{}, err
	}
	adm := Admission{
		RPM:       float64(vals[0]) / 1000,
		TPM:       float64(vals[1]) / 1000,
		SlotTaken: vals[2] == 1,
		Held:      int(vals[3]),
		Release:   func() {},
	}
	if adm.SlotTaken {
		s.keeper.track(slots, member)
		var once sync.Once
		adm.Release = func() {
			once.Do(func() { s.keeper.release(slots, member) })
		}
	}
	return adm, nil
}

// CountRequest counts one request for subject without waiting for Redis. It
// serves subjects that have no rate or concurrency limit and are only counted
// for the dashboard, so their requests never pay a Redis round trip.
func (s *Store) CountRequest(subject string) {
	if s == nil || subject == "" {
		return
	}
	s.counts.add(subject, 1, 0)
}

// AddTokens adds token usage to subject's window without waiting for Redis.
// Tokens are known only after a request completes, so a flush delay of about
// a second is immaterial next to the one-minute window.
func (s *Store) AddTokens(subject string, tokens int64) {
	if s == nil || subject == "" || tokens <= 0 {
		return
	}
	s.counts.add(subject, 0, tokens)
}

// Rates reads the cluster-wide windows of subjects, for the dashboard.
func (s *Store) Rates(ctx context.Context, subjects []string) (map[string]Rate, error) {
	if !s.Available() {
		return nil, sharedredis.ErrUnavailable
	}
	now := s.now()
	minute, elapsed := minuteOf(now)
	weight := float64(windowMillis-elapsed) / float64(windowMillis)
	cmds := make(map[string]*redis.SliceCmd, len(subjects))
	err := s.client.PipelineBackground(ctx, func(pipe redis.Pipeliner) error {
		for _, subject := range subjects {
			rpmCur, rpmPrev, tpmCur, tpmPrev, _ := subjectKeys(subject, minute)
			cmds[subject] = pipe.MGet(context.Background(), rpmCur, rpmPrev, tpmCur, tpmPrev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]Rate, len(subjects))
	for subject, cmd := range cmds {
		vals, errCmd := cmd.Result()
		if errCmd != nil || len(vals) != 4 {
			continue
		}
		out[subject] = Rate{
			RPM: parseCounter(vals[1])*weight + parseCounter(vals[0]),
			TPM: parseCounter(vals[3])*weight + parseCounter(vals[2]),
		}
	}
	return out, nil
}

func parseCounter(v any) float64 {
	str, ok := v.(string)
	if !ok {
		return 0
	}
	n, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0
	}
	return n
}

// countBatcher accumulates asynchronously counted requests and tokens per
// subject and minute, and writes them in one pipeline per flush interval.
type countBatcher struct {
	store    *Store
	interval time.Duration

	mu      sync.Mutex
	pending map[countKey]*countDelta

	startOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
	started   bool
	closeOnce sync.Once
}

type countKey struct {
	subject string
	minute  int64
}

type countDelta struct{ requests, tokens int64 }

func newCountBatcher(s *Store, interval time.Duration) *countBatcher {
	return &countBatcher{
		store:    s,
		interval: interval,
		pending:  make(map[countKey]*countDelta),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (b *countBatcher) add(subject string, requests, tokens int64) {
	b.startOnce.Do(func() {
		b.mu.Lock()
		b.started = true
		b.mu.Unlock()
		go b.run()
	})
	minute, _ := minuteOf(b.store.now())
	key := countKey{subject: subject, minute: minute}
	b.mu.Lock()
	delta := b.pending[key]
	if delta == nil {
		delta = &countDelta{}
		b.pending[key] = delta
	}
	delta.requests += requests
	delta.tokens += tokens
	b.mu.Unlock()
}

func (b *countBatcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			b.flush()
			return
		case <-ticker.C:
			b.flush()
		}
	}
}

// flush writes the accumulated deltas. When Redis is unavailable they are
// dropped: those requests and tokens are still in the node-local windows,
// which are what limits fall back to while Redis is down.
func (b *countBatcher) flush() {
	b.mu.Lock()
	batch := b.pending
	b.pending = make(map[countKey]*countDelta)
	b.mu.Unlock()
	if len(batch) == 0 || !b.store.Available() {
		return
	}
	ctx := context.Background()
	_ = b.store.client.TxPipelineBackground(ctx, func(pipe redis.Pipeliner) error {
		for key, delta := range batch {
			rpmCur, _, tpmCur, _, _ := subjectKeys(key.subject, key.minute)
			if delta.requests > 0 {
				pipe.IncrBy(ctx, rpmCur, delta.requests)
				pipe.PExpire(ctx, rpmCur, bucketTTL)
			}
			if delta.tokens > 0 {
				pipe.IncrBy(ctx, tpmCur, delta.tokens)
				pipe.PExpire(ctx, tpmCur, bucketTTL)
			}
		}
		return nil
	})
}

func (b *countBatcher) close() {
	b.closeOnce.Do(func() {
		close(b.stop)
		b.mu.Lock()
		started := b.started
		b.mu.Unlock()
		if started {
			<-b.done
		}
	})
}
