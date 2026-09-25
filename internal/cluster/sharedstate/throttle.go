package sharedstate

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
)

// Shared login throttle buckets. The semantics are those of the node-local
// throttle in the management handlers, moved into one script so that a
// credential-guessing run spread over several nodes is counted once:
//
//   - two sliding windows (short burst, long daily ceiling), each counted in
//     sub-buckets so memory stays flat under attack;
//   - a quiet period (reset-after) clears counters and escalation;
//   - failures arriving while a block is armed are refused without being
//     counted;
//   - tripping either window arms a block whose length climbs a ladder
//     indexed by how many blocks came before.
//
// The bucket is a hash: s:<n>/l:<n> are short/long sub-bucket counts keyed by
// absolute sub-bucket number, bu is blocked-until, g the escalation
// generation, la the last activity (all times in ms).
//
// KEYS[1] bucket
// ARGV[1] "charge" or "peek"   ARGV[2] now
// ARGV[3..5] short limit, sub-bucket width (ms), sub-bucket count
// ARGV[6..8] long limit, sub-bucket width (ms), sub-bucket count
// ARGV[9] reset-after (ms, 0 = never)   ARGV[10] retention (ms)
// ARGV[11..] block durations (ms) by generation
// Returns {blocked (0/1), retry after (ms), generation, short count, long count, newly armed (0/1)}.
var throttleScript = redis.NewScript(`
local key = KEYS[1]
local charge = ARGV[1] == 'charge'
local now = tonumber(ARGV[2])
local windows = {
  {prefix = 's:', limit = tonumber(ARGV[3]), width = tonumber(ARGV[4]), slots = tonumber(ARGV[5])},
  {prefix = 'l:', limit = tonumber(ARGV[6]), width = tonumber(ARGV[7]), slots = tonumber(ARGV[8])},
}
local resetAfter = tonumber(ARGV[9])
local retention = tonumber(ARGV[10])
local ladder = #ARGV - 10

local fields = {}
local raw = redis.call('HGETALL', key)
for i = 1, #raw, 2 do
  fields[raw[i]] = raw[i + 1]
end

local lastActivity = tonumber(fields['la'] or '0')
if charge and resetAfter > 0 and lastActivity > 0 and now - lastActivity >= resetAfter then
  redis.call('DEL', key)
  fields = {}
end
local blockedUntil = tonumber(fields['bu'] or '0')
local generation = tonumber(fields['g'] or '0')

local function count(w)
  if w.limit <= 0 or w.width <= 0 then
    return 0
  end
  local current = math.floor(now / w.width)
  local oldest = current - w.slots + 1
  local total = 0
  for field, value in pairs(fields) do
    if string.sub(field, 1, 2) == w.prefix then
      local index = tonumber(string.sub(field, 3))
      if index and index >= oldest and index <= current then
        total = total + tonumber(value)
      elseif charge and index and index < oldest then
        redis.call('HDEL', key, field)
      end
    end
  end
  return total
end

local function keep(extra)
  local ttl = retention
  if extra > ttl then
    ttl = extra
  end
  redis.call('PEXPIRE', key, ttl)
end

if not charge then
  if blockedUntil > now then
    return {1, blockedUntil - now, generation, count(windows[1]), count(windows[2]), 0}
  end
  return {0, 0, 0, 0, 0, 0}
end

redis.call('HSET', key, 'la', now)
if blockedUntil > now then
  keep(blockedUntil - now)
  return {1, blockedUntil - now, generation, count(windows[1]), count(windows[2]), 0}
end

for _, w in ipairs(windows) do
  if w.limit > 0 and w.width > 0 then
    local field = w.prefix .. math.floor(now / w.width)
    fields[field] = tostring(redis.call('HINCRBY', key, field, 1))
  end
end
local shortCount = count(windows[1])
local longCount = count(windows[2])
local tripped = (windows[1].limit > 0 and shortCount >= windows[1].limit) or
  (windows[2].limit > 0 and longCount >= windows[2].limit)
if not tripped or ladder < 1 then
  keep(0)
  return {0, 0, generation, shortCount, longCount, 0}
end
local rung = generation + 1
if rung > ladder then
  rung = ladder
end
local wait = tonumber(ARGV[10 + rung])
if wait <= 0 then
  keep(0)
  return {0, 0, generation, shortCount, longCount, 0}
end
generation = generation + 1
redis.call('HSET', key, 'bu', now + wait, 'g', generation)
keep(wait)
return {1, wait, generation, shortCount, longCount, 1}
`)

// ThrottleWindow is one sliding window of a throttle bucket. A zero Limit
// disables it.
type ThrottleWindow struct {
	Limit  int
	Window time.Duration
	Slots  int
}

// ThrottleSpec is a bucket's policy.
type ThrottleSpec struct {
	Short      ThrottleWindow
	Long       ThrottleWindow
	Backoff    []time.Duration
	ResetAfter time.Duration
	// Retention is how long an idle bucket is kept; a live block always
	// extends it to the block's end.
	Retention time.Duration
}

// ThrottleState is a bucket's standing after a charge or peek.
type ThrottleState struct {
	Blocked    bool
	RetryAfter time.Duration
	Generation int
	ShortCount int
	LongCount  int
	NewlyArmed bool
}

func throttleKey(bucket string) string {
	return sharedredis.KeyPrefix + "throttle:" + sharedredis.HashPart(bucket)
}

// ThrottleCharge charges one failure against bucket.
func (s *Store) ThrottleCharge(ctx context.Context, bucket string, spec ThrottleSpec, now time.Time) (ThrottleState, error) {
	return s.throttle(ctx, "charge", bucket, spec, now)
}

// ThrottlePeek reports a bucket's standing without charging it.
func (s *Store) ThrottlePeek(ctx context.Context, bucket string, spec ThrottleSpec, now time.Time) (ThrottleState, error) {
	return s.throttle(ctx, "peek", bucket, spec, now)
}

// ThrottleClear deletes a bucket, as a successful login does.
func (s *Store) ThrottleClear(ctx context.Context, bucket string) error {
	if !s.Available() {
		return sharedredis.ErrUnavailable
	}
	_, err := s.client.Eval(ctx, deleteKeyScript, []string{throttleKey(bucket)})
	return err
}

var deleteKeyScript = redis.NewScript(`return redis.call('DEL', KEYS[1])`)

func (s *Store) throttle(ctx context.Context, mode, bucket string, spec ThrottleSpec, now time.Time) (ThrottleState, error) {
	if !s.Available() {
		return ThrottleState{}, sharedredis.ErrUnavailable
	}
	args := []any{
		mode, now.UnixMilli(),
		spec.Short.Limit, slotWidth(spec.Short).Milliseconds(), spec.Short.Slots,
		spec.Long.Limit, slotWidth(spec.Long).Milliseconds(), spec.Long.Slots,
		spec.ResetAfter.Milliseconds(), spec.Retention.Milliseconds(),
	}
	for _, step := range spec.Backoff {
		args = append(args, step.Milliseconds())
	}
	res, err := s.client.Eval(ctx, throttleScript, []string{throttleKey(bucket)}, args...)
	if err != nil {
		return ThrottleState{}, err
	}
	vals, err := int64s(res, 6)
	if err != nil {
		return ThrottleState{}, err
	}
	return ThrottleState{
		Blocked:    vals[0] == 1,
		RetryAfter: time.Duration(vals[1]) * time.Millisecond,
		Generation: int(vals[2]),
		ShortCount: int(vals[3]),
		LongCount:  int(vals[4]),
		NewlyArmed: vals[5] == 1,
	}, nil
}

// slotWidth is Window/Slots, or zero when the window is disabled. A width
// below one millisecond is rounded up so the script never divides by zero.
func slotWidth(w ThrottleWindow) time.Duration {
	if w.Limit <= 0 || w.Window <= 0 || w.Slots <= 0 {
		return 0
	}
	width := w.Window / time.Duration(w.Slots)
	if width < time.Millisecond {
		width = time.Millisecond
	}
	return width
}
