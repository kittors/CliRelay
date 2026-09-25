package clusterruntime

import (
	"context"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// cooldownRelay carries upstream cooldowns between the auth managers of
// different nodes over the cluster bus (TopicCooldown).
//
// Both directions go through a queue and a worker. Publishing is a database
// round trip that must not delay the request whose failure caused it, and
// applying a peer's cooldown takes the manager lock, which a slow credential
// write can hold; the bus goroutine delivering the event must never wait on
// either.
type cooldownRelay struct {
	coord func() *cluster.Coordinator

	mu        sync.Mutex
	published map[cooldownKey]time.Time
	managers  []*coreauth.Manager

	outbound    chan coreauth.CooldownNotice
	inbound     chan cluster.Event
	stop        chan struct{}
	workers     sync.WaitGroup
	unsubscribe func()
	closeOnce   sync.Once

	dropMu     sync.Mutex
	lastDropAt time.Time
}

type cooldownKey struct{ authID, model string }

const (
	// cooldownRepublishSlack keeps a burst of failures on one account from
	// sending an event per failure: an announced cooldown is announced again
	// only when it moves at least this much later.
	cooldownRepublishSlack = 5 * time.Second
	cooldownQueueSize      = 1024
	cooldownPublishTimeout = 5 * time.Second
	// cooldownPruneAt bounds the announced-deadline map; entries whose
	// deadline passed are dropped once it grows this large.
	cooldownPruneAt = 4096
)

func newCooldownRelay(coord func() *cluster.Coordinator) *cooldownRelay {
	return &cooldownRelay{
		coord:     coord,
		published: make(map[cooldownKey]time.Time),
		outbound:  make(chan coreauth.CooldownNotice, cooldownQueueSize),
		inbound:   make(chan cluster.Event, cooldownQueueSize),
		stop:      make(chan struct{}),
	}
}

// start subscribes on c and starts the workers. c must be the coordinator
// Start adopts (see cluster.Prepare); a subscription on any other one would
// never fire.
func (r *cooldownRelay) start(c *cluster.Coordinator) {
	r.unsubscribe = c.Subscribe(cluster.TopicCooldown, r.receive)
	r.workers.Add(2)
	go r.runOutbound()
	go r.runInbound()
}

// attach makes m publish its cooldowns and apply its peers'.
func (r *cooldownRelay) attach(m *coreauth.Manager) {
	r.mu.Lock()
	for _, existing := range r.managers {
		if existing == m {
			r.mu.Unlock()
			return
		}
	}
	r.managers = append(r.managers, m)
	r.mu.Unlock()
	m.SetCooldownPublisher(r.publish)
}

// publish is the managers' cooldown publisher. It never blocks.
func (r *cooldownRelay) publish(n coreauth.CooldownNotice) {
	if c := r.coord(); c == nil || !c.Enabled() {
		return
	}
	if !r.claim(n) {
		return
	}
	select {
	case r.outbound <- n:
	default:
		r.noteDrop("outbound")
	}
}

// claim reports whether n moves the announced deadline of its (account,
// model) by more than the slack, and records it if so.
func (r *cooldownRelay) claim(n coreauth.CooldownNotice) bool {
	key := cooldownKey{authID: n.AuthID, model: n.Model}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.published[key]; ok && prev.After(now) && !n.Until.After(prev.Add(cooldownRepublishSlack)) {
		return false
	}
	if len(r.published) >= cooldownPruneAt {
		for k, until := range r.published {
			if !until.After(now) {
				delete(r.published, k)
			}
		}
	}
	r.published[key] = n.Until
	return true
}

// receive is the bus callback. A Resync needs no work: cooldowns are not
// reloadable state, a missed one only means this node finds out by itself.
func (r *cooldownRelay) receive(ev cluster.Event) {
	if ev.Resync || ev.Topic != cluster.TopicCooldown {
		return
	}
	select {
	case r.inbound <- ev:
	default:
		r.noteDrop("inbound")
	}
}

func (r *cooldownRelay) runOutbound() {
	defer r.workers.Done()
	for {
		select {
		case <-r.stop:
			return
		case n := <-r.outbound:
			r.send(n)
		}
	}
}

func (r *cooldownRelay) send(n coreauth.CooldownNotice) {
	c := r.coord()
	if c == nil || !c.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cooldownPublishTimeout)
	defer cancel()
	ev := cluster.CooldownEvent{
		AuthID: n.AuthID,
		Model:  n.Model,
		// Rounded up to the second: a receiver may cool slightly longer than
		// the reporter, never shorter.
		UntilUnix:  (n.Until.UnixMilli() + 999) / 1000,
		StatusCode: n.StatusCode,
		Reason:     n.Reason,
		Quota:      n.Quota,
	}
	if err := c.Publish(ctx, cluster.TopicCooldown, ev); err != nil {
		log.WithError(err).WithField("auth_id", n.AuthID).Debug("cluster: publish cooldown failed")
	}
}

func (r *cooldownRelay) runInbound() {
	defer r.workers.Done()
	for {
		select {
		case <-r.stop:
			return
		case ev := <-r.inbound:
			r.apply(ev)
		}
	}
}

func (r *cooldownRelay) apply(ev cluster.Event) {
	var payload cluster.CooldownEvent
	if err := ev.Decode(&payload); err != nil || payload.AuthID == "" || payload.UntilUnix <= 0 {
		return
	}
	notice := coreauth.CooldownNotice{
		AuthID:     payload.AuthID,
		Model:      payload.Model,
		Until:      time.Unix(payload.UntilUnix, 0),
		StatusCode: payload.StatusCode,
		Reason:     payload.Reason,
		Quota:      payload.Quota,
		Origin:     ev.Origin,
	}
	r.mu.Lock()
	managers := append([]*coreauth.Manager(nil), r.managers...)
	r.mu.Unlock()
	for _, m := range managers {
		m.ApplyPeerCooldown(notice)
	}
}

func (r *cooldownRelay) noteDrop(direction string) {
	r.dropMu.Lock()
	defer r.dropMu.Unlock()
	if time.Since(r.lastDropAt) < time.Minute {
		return
	}
	r.lastDropAt = time.Now()
	log.Warnf("cluster: %s cooldown queue full; dropping events (each node still learns cooldowns from its own failures)", direction)
}

func (r *cooldownRelay) close() {
	r.closeOnce.Do(func() {
		if r.unsubscribe != nil {
			r.unsubscribe()
		}
		close(r.stop)
		r.workers.Wait()
		r.mu.Lock()
		managers := r.managers
		r.managers = nil
		r.mu.Unlock()
		for _, m := range managers {
			m.SetCooldownPublisher(nil)
		}
	})
}
