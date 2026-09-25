package sharedstate

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
)

// Shared auto-ban counters. A source failing authentication on several nodes
// is counted once, and exactly one node acts when it crosses the threshold.
//
// A plain shared counter is not enough for the second part: two nodes can
// both see the count cross the threshold, and each would write the deny rule,
// send the alert and bump the escalation counter. So crossing the threshold
// also takes a short claim; while it is held other nodes treat the source as
// already banned. The claiming node then writes the rule and marks the ban,
// which releases the claim. If it fails to (a database error, a manual rule
// already in place) the claim simply lapses and the next failure retries,
// which is what the node-local engine does too.
//
// The source is a hash: s:<n> sub-bucket counts, bc bans so far (drives the
// doubling ban length and survives the window), bu banned-until, cl
// claimed-until, la last activity (ms).

// autoBanChargeScript charges one failure.
//
// KEYS[1] source
// ARGV[1] now (ms)   ARGV[2] sub-bucket width (ms)   ARGV[3] sub-bucket count
// ARGV[4] threshold   ARGV[5] claim TTL (ms)   ARGV[6] retention (ms)
// Returns {failures in window, bans so far, already banned or claimed (0/1), claimed now (0/1)}.
var autoBanChargeScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local width = tonumber(ARGV[2])
local slots = tonumber(ARGV[3])
local threshold = tonumber(ARGV[4])
local retention = tonumber(ARGV[6])

local fields = {}
local raw = redis.call('HGETALL', key)
for i = 1, #raw, 2 do
  fields[raw[i]] = raw[i + 1]
end
local bans = tonumber(fields['bc'] or '0')
local bannedUntil = tonumber(fields['bu'] or '0')
local claimUntil = tonumber(fields['cl'] or '0')

local function keep(extra)
  local ttl = retention
  if extra > ttl then
    ttl = extra
  end
  redis.call('PEXPIRE', key, ttl)
end

redis.call('HSET', key, 'la', now)
if bannedUntil > now or claimUntil > now then
  local horizon = bannedUntil
  if claimUntil > horizon then
    horizon = claimUntil
  end
  keep(horizon - now)
  return {0, bans, 1, 0}
end

local current = math.floor(now / width)
local oldest = current - slots + 1
local field = 's:' .. current
fields[field] = tostring(redis.call('HINCRBY', key, field, 1))
local total = 0
for name, value in pairs(fields) do
  if string.sub(name, 1, 2) == 's:' then
    local index = tonumber(string.sub(name, 3))
    if index and index >= oldest and index <= current then
      total = total + tonumber(value)
    elseif index and index < oldest then
      redis.call('HDEL', key, name)
    end
  end
end

local claimed = 0
if total >= threshold then
  redis.call('HSET', key, 'cl', now + tonumber(ARGV[5]))
  claimed = 1
end
keep(0)
return {total, bans, 0, claimed}
`)

// autoBanMarkScript records a ban: it counts it for escalation, clears the
// window so the same failures cannot re-trigger once it lapses, and releases
// the claim.
//
// KEYS[1] source   ARGV[1] banned until (ms)   ARGV[2] now (ms)   ARGV[3] retention (ms)
var autoBanMarkScript = redis.NewScript(`
local key = KEYS[1]
for _, name in ipairs(redis.call('HKEYS', key)) do
  if string.sub(name, 1, 2) == 's:' then
    redis.call('HDEL', key, name)
  end
end
redis.call('HDEL', key, 'cl')
redis.call('HINCRBY', key, 'bc', 1)
redis.call('HSET', key, 'bu', ARGV[1], 'la', ARGV[2])
local ttl = tonumber(ARGV[3])
local extra = tonumber(ARGV[1]) - tonumber(ARGV[2])
if extra > ttl then
  ttl = extra
end
redis.call('PEXPIRE', key, ttl)
return 1
`)

// AutoBanSpec is the part of the auto-ban policy the counter needs.
type AutoBanSpec struct {
	Window    time.Duration
	Slots     int
	Threshold int
	// ClaimTTL bounds how long one node may take to act on a crossing.
	ClaimTTL time.Duration
	// Retention keeps an idle source's ban history for escalation.
	Retention time.Duration
}

// AutoBanCharge is the result of charging one failure.
type AutoBanCharge struct {
	Failures int
	Bans     int
	// AlreadyBanned is true while a ban or another node's claim is in force;
	// the failure was not counted.
	AlreadyBanned bool
	// Claimed is true when this failure crossed the threshold and this node
	// must now act on it.
	Claimed bool
}

func autoBanKey(cidr string) string {
	// Kept readable: an operator clearing one source's history by hand needs
	// to find it, and a network prefix is not a secret.
	return sharedredis.KeyPrefix + "autoban:" + cidr
}

// AutoBanCharge charges one authentication failure against cidr.
func (s *Store) AutoBanCharge(ctx context.Context, cidr string, spec AutoBanSpec, now time.Time) (AutoBanCharge, error) {
	if !s.Available() {
		return AutoBanCharge{}, sharedredis.ErrUnavailable
	}
	width := time.Millisecond
	if spec.Slots > 0 && spec.Window/time.Duration(spec.Slots) > width {
		width = spec.Window / time.Duration(spec.Slots)
	}
	res, err := s.client.Eval(ctx, autoBanChargeScript, []string{autoBanKey(cidr)},
		now.UnixMilli(), width.Milliseconds(), spec.Slots, spec.Threshold,
		spec.ClaimTTL.Milliseconds(), spec.Retention.Milliseconds())
	if err != nil {
		return AutoBanCharge{}, err
	}
	vals, err := int64s(res, 4)
	if err != nil {
		return AutoBanCharge{}, err
	}
	return AutoBanCharge{
		Failures:      int(vals[0]),
		Bans:          int(vals[1]),
		AlreadyBanned: vals[2] == 1,
		Claimed:       vals[3] == 1,
	}, nil
}

// AutoBanMark records that cidr is banned until until.
func (s *Store) AutoBanMark(ctx context.Context, cidr string, until, now time.Time, retention time.Duration) error {
	if !s.Available() {
		return sharedredis.ErrUnavailable
	}
	_, err := s.client.Eval(ctx, autoBanMarkScript, []string{autoBanKey(cidr)},
		until.UnixMilli(), now.UnixMilli(), retention.Milliseconds())
	return err
}
