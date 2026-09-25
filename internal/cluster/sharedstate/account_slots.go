package sharedstate

import (
	"context"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
)

// accountSlotPool holds this node's cluster-wide slots on upstream accounts.
//
// The account limiter hands a finished request's slot straight to a queued
// request on the same node, so slots are fungible per account: what matters to
// the cluster is how many this node holds, not which request holds which one.
// The pool is therefore a per-account stack of lease members. A handoff never
// touches Redis; only a slot that is really given up pops a member.
type accountSlotPool struct {
	store *Store

	mu      sync.Mutex
	members map[string][]string
}

func newAccountSlotPool(s *Store) *accountSlotPool {
	return &accountSlotPool{store: s, members: make(map[string][]string)}
}

func accountSlotKey(authID string) string {
	return sharedredis.KeyPrefix + "acct:{" + sharedredis.HashPart(authID) + "}:conc"
}

// AcquireAccountSlot takes one cluster-wide slot on an upstream account when
// fewer than limit are held across the cluster. It returns the number held
// after the call.
func (s *Store) AcquireAccountSlot(ctx context.Context, authID string, limit int) (bool, int, error) {
	if !s.Available() {
		return false, 0, sharedredis.ErrUnavailable
	}
	key := accountSlotKey(authID)
	member := s.newLeaseMember()
	ok, held, err := s.acquireLease(ctx, key, member, limit)
	if err != nil || !ok {
		return ok, held, err
	}
	s.slots.mu.Lock()
	s.slots.members[authID] = append(s.slots.members[authID], member)
	s.slots.mu.Unlock()
	s.keeper.track(key, member)
	return true, held, nil
}

// ReleaseAccountSlot gives back one of this node's slots on authID without
// blocking. It is a no-op when the node holds none.
func (s *Store) ReleaseAccountSlot(authID string) {
	if s == nil {
		return
	}
	s.slots.mu.Lock()
	list := s.slots.members[authID]
	if len(list) == 0 {
		s.slots.mu.Unlock()
		return
	}
	member := list[len(list)-1]
	if len(list) == 1 {
		delete(s.slots.members, authID)
	} else {
		s.slots.members[authID] = list[:len(list)-1]
	}
	s.slots.mu.Unlock()
	s.keeper.release(accountSlotKey(authID), member)
}

// AccountSlotsHeld reports how many cluster-wide slots this node holds on
// authID. Test-facing.
func (s *Store) AccountSlotsHeld(authID string) int {
	if s == nil {
		return 0
	}
	s.slots.mu.Lock()
	defer s.slots.mu.Unlock()
	return len(s.slots.members[authID])
}
