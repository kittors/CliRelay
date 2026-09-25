package sharedstate

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
)

// Session affinity bindings map a session key to the upstream account serving
// that conversation. Keeping them in the shared Redis is what keeps a
// conversation on one account, and so on that account's prompt cache, when
// its turns land on different nodes.
//
// A binding is a hash: field a names the account (the caller passes an opaque
// reference, never a raw auth ID), field n counts the requests it has served,
// which the selector compares with the group's sticky-max-requests to bound
// how long one conversation pins one account.

// affinityLookupScript returns the binding and counts this request against it,
// refreshing the TTL: reading a binding is what keeps it alive.
//
// KEYS[1] binding   ARGV[1] TTL (ms)
// Returns {} when unbound, else {account, requests served before this one}.
var affinityLookupScript = redis.NewScript(`
local account = redis.call('HGET', KEYS[1], 'a')
if not account then
  return {}
end
local served = redis.call('HINCRBY', KEYS[1], 'n', 1)
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return {account, served - 1}
`)

// affinityBindScript creates the binding unless one exists. On a conflict the
// existing binding wins, exactly like SET NX, and counts this request.
//
// KEYS[1] binding   ARGV[1] account   ARGV[2] TTL (ms)
// Returns {winning account, requests served before this one, created (0/1)}.
var affinityBindScript = redis.NewScript(`
local account = redis.call('HGET', KEYS[1], 'a')
if account then
  local served = redis.call('HINCRBY', KEYS[1], 'n', 1)
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return {account, served - 1, 0}
end
redis.call('HSET', KEYS[1], 'a', ARGV[1], 'n', 1)
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return {ARGV[1], 0, 1}
`)

// affinityReleaseScript deletes the binding only if it still names ARGV[1], so
// a node releasing a stale binding cannot delete the fresh one another node
// just created.
var affinityReleaseScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'a') == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// Binding is a shared session affinity binding.
type Binding struct {
	// Account is the reference the binding was created with.
	Account string
	// Served counts the requests the binding served before this one.
	Served int
	// Found is false when no binding existed.
	Found bool
	// Created is true when Bind created the binding rather than finding one.
	Created bool
}

func affinityKey(sessionKey string) string {
	return sharedredis.KeyPrefix + "sticky:" + sharedredis.HashPart(sessionKey)
}

// AffinityLookup returns the binding for sessionKey, counting this request
// and extending its TTL.
func (s *Store) AffinityLookup(ctx context.Context, sessionKey string, ttl time.Duration) (Binding, error) {
	if !s.Available() {
		return Binding{}, sharedredis.ErrUnavailable
	}
	res, err := s.client.Eval(ctx, affinityLookupScript, []string{affinityKey(sessionKey)}, ttl.Milliseconds())
	if err != nil {
		return Binding{}, err
	}
	list, ok := res.([]any)
	if !ok {
		return Binding{}, fmt.Errorf("sharedstate: unexpected affinity reply %T", res)
	}
	if len(list) == 0 {
		return Binding{}, nil
	}
	return parseBinding(list, false)
}

// AffinityBind binds sessionKey to account unless a binding exists, in which
// case the existing one is returned and counts this request.
func (s *Store) AffinityBind(ctx context.Context, sessionKey, account string, ttl time.Duration) (Binding, error) {
	if !s.Available() {
		return Binding{}, sharedredis.ErrUnavailable
	}
	res, err := s.client.Eval(ctx, affinityBindScript, []string{affinityKey(sessionKey)}, account, ttl.Milliseconds())
	if err != nil {
		return Binding{}, err
	}
	list, ok := res.([]any)
	if !ok || len(list) < 3 {
		return Binding{}, fmt.Errorf("sharedstate: unexpected affinity reply %T %v", res, res)
	}
	return parseBinding(list, true)
}

// AffinityRelease deletes the binding for sessionKey if it still names account.
func (s *Store) AffinityRelease(ctx context.Context, sessionKey, account string) error {
	if !s.Available() {
		return sharedredis.ErrUnavailable
	}
	_, err := s.client.Eval(ctx, affinityReleaseScript, []string{affinityKey(sessionKey)}, account)
	return err
}

func parseBinding(list []any, withCreated bool) (Binding, error) {
	account, ok := list[0].(string)
	if !ok || len(list) < 2 {
		return Binding{}, fmt.Errorf("sharedstate: unexpected affinity reply %v", list)
	}
	served, _ := list[1].(int64)
	b := Binding{Account: account, Served: int(served), Found: true}
	if withCreated {
		created, _ := list[2].(int64)
		b.Created = created == 1
	}
	return b, nil
}
