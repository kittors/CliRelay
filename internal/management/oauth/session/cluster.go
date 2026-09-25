package session

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// Status values of a shared session row.
const (
	RecordPending   = "pending"
	RecordSuccess   = "success"
	RecordError     = "error"
	RecordCancelled = "cancelled"
	RecordExpired   = "expired"
)

const (
	// OwnerHeartbeatInterval is how often the node driving a login records
	// that it is still alive.
	OwnerHeartbeatInterval = 15 * time.Second
	// OwnerLostAfter is how long a pending session may go without a heartbeat
	// before every node treats its owner as gone.
	OwnerLostAfter = 60 * time.Second
	// CallbackGrace keeps a session that has received its callback alive long
	// enough for the owner to exchange the code, even when the callback arrived
	// at the last moment.
	CallbackGrace = 2 * time.Minute

	defaultClusterPollInterval = 2 * time.Second
	repoTimeout                = 5 * time.Second
)

// Messages recorded on shared sessions that end without the owner finishing
// them.
const (
	MessageOwnerLost  = "The server node handling this login stopped before it finished; start the login again"
	MessageSuperseded = "Superseded by another completed login for the same provider"
	MessageExpired    = "OAuth session expired; start the login again"
)

// Record is one shared session row.
type Record struct {
	State     string
	Provider  string
	TenantID  string
	Status    string
	Error     string
	OwnerNode string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Expired and OwnerLost are judged by the repository against its own
	// clock. Nodes compare against the database's time, the only clock they
	// all share, rather than against their own.
	Expired   bool
	OwnerLost bool
}

// Repo persists shared sessions. The PostgreSQL implementation lives in
// internal/clusterstate; MemoryRepo serves tests.
type Repo interface {
	// Create inserts a pending session, replacing any row with the same state.
	Create(ctx context.Context, rec Record, ttl time.Duration) error
	// Get returns the session row, or false when there is none.
	Get(ctx context.Context, state string) (Record, bool, error)
	// Deliver stores a provider callback on a live pending session whose
	// owner is still heartbeating, and extends its expiry by CallbackGrace.
	// It reports false when the session is not in that state.
	Deliver(ctx context.Context, state, provider string, payload map[string]string) (bool, error)
	// TakeCallback returns and clears the callback of a pending session.
	TakeCallback(ctx context.Context, state string) (map[string]string, bool, error)
	// Finish moves a pending session to status and restarts its retention
	// window. It reports false when the session was not pending.
	Finish(ctx context.Context, state, status, message string, ttl time.Duration) (bool, error)
	// CancelPending cancels the live pending sessions of provider, only those
	// of tenantID when it is set, and returns their states.
	CancelPending(ctx context.Context, provider, tenantID, message string, ttl time.Duration) ([]string, error)
	// Heartbeat records that owner still drives the pending session. It
	// reports false once the session is no longer pending.
	Heartbeat(ctx context.Context, state, owner string) (bool, error)
}

// ClusterStore shares OAuth sessions between the nodes of a cluster.
//
// The PKCE verifier and nonce of a login never leave the node that started
// it: they stay in that node's flow goroutine, so only that node can exchange
// the code. Everything around the exchange is shared. Any node may receive the
// provider callback, as a browser redirect or as a URL pasted into the panel;
// it stores the payload on the session row and announces it on
// cluster.TopicOAuth, which wakes the owner's waiter. The owner exchanges the
// code, saves the credential and records the outcome, and every node answers
// status polls from the row.
//
// If the owner dies mid-login the verifier dies with it and the login cannot
// be finished anywhere. Owners therefore heartbeat their pending sessions, and
// once a heartbeat is older than OwnerLostAfter every node reports the session
// as failed, so the user starts over instead of waiting out the expiry.
type ClusterStore struct {
	repo  Repo
	coord func() *cluster.Coordinator
	ttl   time.Duration

	mu           sync.Mutex
	pollInterval time.Duration
	beatInterval time.Duration
	waiters      map[string]*waiter
	heartbeats   map[string]*heartbeat
	subscribedTo *cluster.Coordinator
	unsubscribe  func()
}

type waiter struct {
	wake chan struct{}
	refs int
}

type heartbeat struct {
	cancel context.CancelFunc
}

// NewClusterStore returns a shared session store over repo. coord supplies
// this node's coordinator; nil means cluster.Default.
func NewClusterStore(repo Repo, coord func() *cluster.Coordinator, ttl time.Duration) *ClusterStore {
	if coord == nil {
		coord = cluster.Default
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &ClusterStore{
		repo:         repo,
		coord:        coord,
		ttl:          ttl,
		pollInterval: defaultClusterPollInterval,
		beatInterval: OwnerHeartbeatInterval,
		waiters:      make(map[string]*waiter),
		heartbeats:   make(map[string]*heartbeat),
	}
}

// SetPollInterval changes how often a waiting owner re-reads its session when
// no event arrives. Events normally wake it first; the poll covers lost
// notifications.
func (s *ClusterStore) SetPollInterval(interval time.Duration) {
	if s == nil || interval <= 0 {
		return
	}
	s.mu.Lock()
	s.pollInterval = interval
	s.mu.Unlock()
}

// SetHeartbeatInterval changes how often owned pending sessions heartbeat.
func (s *ClusterStore) SetHeartbeatInterval(interval time.Duration) {
	if s == nil || interval <= 0 {
		return
	}
	s.mu.Lock()
	s.beatInterval = interval
	s.mu.Unlock()
}

// Close stops heartbeats and leaves the event bus. Sessions still pending
// here are reported as failed by the other nodes once their heartbeat ages
// out, which is what a real shutdown looks like to them.
func (s *ClusterStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	for state, beat := range s.heartbeats {
		beat.cancel()
		delete(s.heartbeats, state)
	}
	unsubscribe := s.unsubscribe
	s.unsubscribe = nil
	s.subscribedTo = nil
	s.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
}

func (s *ClusterStore) RegisterTenant(state, provider, tenantID string) {
	if s == nil {
		return
	}
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if state == "" || provider == "" {
		return
	}
	// Subscribe before the URL goes out, so no callback event can arrive
	// before this node listens for it.
	s.ensureSubscribed()
	owner := s.coord().NodeID()
	ctx, cancel := context.WithTimeout(context.Background(), repoTimeout)
	defer cancel()
	err := s.repo.Create(ctx, Record{
		State:     state,
		Provider:  provider,
		TenantID:  strings.TrimSpace(tenantID),
		Status:    RecordPending,
		OwnerNode: owner,
	}, s.ttl)
	if err != nil {
		// The flow has no error path at this point; the login surfaces as an
		// unknown state on the first status poll.
		log.WithError(err).WithField("provider", provider).Error("oauth: failed to record shared login session")
		return
	}
	s.startHeartbeat(state, owner)
}

func (s *ClusterStore) SetError(state, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Authentication failed"
	}
	s.finish(state, RecordError, message)
}

func (s *ClusterStore) Complete(state string) {
	s.finish(state, RecordSuccess, "")
}

func (s *ClusterStore) finish(state, status, message string) {
	if s == nil {
		return
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	s.stopHeartbeat(state)
	ctx, cancel := context.WithTimeout(context.Background(), repoTimeout)
	defer cancel()
	if _, err := s.repo.Finish(ctx, state, status, message, s.ttl); err != nil {
		log.WithError(err).Errorf("oauth: failed to record %s for shared login session", status)
	}
}

// CompleteProviderTenant cancels the other pending logins of provider, as the
// memory store does by deleting them. Their owners wake and give up.
//
// Failed sessions are left as they are. The memory store deletes those too,
// which turns a reported failure into an unknown state; keeping the failure
// is what a poll of that state should see.
func (s *ClusterStore) CompleteProviderTenant(provider, tenantID string) int {
	if s == nil {
		return 0
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), repoTimeout)
	defer cancel()
	states, err := s.repo.CancelPending(ctx, provider, strings.TrimSpace(tenantID), MessageSuperseded, s.ttl)
	if err != nil {
		log.WithError(err).WithField("provider", provider).Warn("oauth: failed to cancel superseded login sessions")
		return 0
	}
	for _, state := range states {
		s.stopHeartbeat(state)
		s.announce(state)
	}
	return len(states)
}

func (s *ClusterStore) Get(state string) (Session, bool) {
	rec, ok, err := s.read(state)
	if err != nil {
		log.WithError(err).Warn("oauth: failed to read shared login session")
		return Session{}, false
	}
	if !ok {
		return Session{}, false
	}
	return sessionFromRecord(rec)
}

func (s *ClusterStore) IsPending(state, provider string) bool {
	session, ok := s.Get(state)
	if !ok || session.Status != "" {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	return provider == "" || strings.EqualFold(session.Provider, provider)
}

// sessionFromRecord maps a row onto the memory store's contract: an empty
// status is pending, StatusCompleted is success and any other status is the
// failure message. Cancelled and expired sessions do not exist there, because
// the memory store deletes them.
func sessionFromRecord(rec Record) (Session, bool) {
	if rec.Expired {
		return Session{}, false
	}
	session := Session{
		Provider:  rec.Provider,
		TenantID:  rec.TenantID,
		CreatedAt: rec.CreatedAt,
		ExpiresAt: rec.ExpiresAt,
	}
	switch rec.Status {
	case RecordPending:
		if rec.OwnerLost {
			session.Status = MessageOwnerLost
		}
	case RecordSuccess:
		session.Status = StatusCompleted
	case RecordError:
		session.Status = strings.TrimSpace(rec.Error)
		if session.Status == "" {
			session.Status = "Authentication failed"
		}
	default:
		return Session{}, false
	}
	return session, true
}

// LookupOutcome classifies a shared session for status reporting.
type LookupOutcome int

const (
	// LookupMissing means no row exists for the state.
	LookupMissing LookupOutcome = iota
	// LookupFound means the session is live; Session carries its status in
	// the memory store's encoding.
	LookupFound
	// LookupSuperseded means another login of the same provider completed
	// while this one was pending.
	LookupSuperseded
	// LookupExpired means the session outlived its window.
	LookupExpired
)

// LookupResult is what Lookup knows about a state. Session.Provider and
// Session.TenantID are set for every outcome but LookupMissing, so callers
// can apply tenant checks before revealing anything.
type LookupResult struct {
	Outcome LookupOutcome
	Session Session
}

// Lookup is Get for status polls: it tells a state that never existed or was
// cleaned up apart from one that expired or was superseded, and it reports
// read failures instead of treating them as a missing state.
func (s *ClusterStore) Lookup(state string) (LookupResult, error) {
	rec, ok, err := s.read(state)
	if err != nil {
		return LookupResult{}, err
	}
	if !ok {
		return LookupResult{Outcome: LookupMissing}, nil
	}
	base := Session{Provider: rec.Provider, TenantID: rec.TenantID, CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt}
	switch {
	case rec.Status == RecordExpired || rec.Expired:
		return LookupResult{Outcome: LookupExpired, Session: base}, nil
	case rec.Status == RecordCancelled:
		return LookupResult{Outcome: LookupSuperseded, Session: base}, nil
	}
	session, ok := sessionFromRecord(rec)
	if !ok {
		return LookupResult{Outcome: LookupExpired, Session: base}, nil
	}
	return LookupResult{Outcome: LookupFound, Session: session}, nil
}

func (s *ClusterStore) read(state string) (Record, bool, error) {
	if s == nil {
		return Record{}, false, nil
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return Record{}, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), repoTimeout)
	defer cancel()
	return s.repo.Get(ctx, state)
}

// DeliverCallback stores the callback on the session row, whichever node the
// callback reached, and wakes the owner. authDir is unused: nothing is
// written to disk.
func (s *ClusterStore) DeliverCallback(_ string, provider, state, code, errorMessage string) error {
	canonicalProvider, err := NormalizeProvider(provider)
	if err != nil {
		return err
	}
	if err := ValidateState(state); err != nil {
		return err
	}
	state = strings.TrimSpace(state)
	payload := map[string]string{
		"code":  strings.TrimSpace(code),
		"state": state,
		"error": strings.TrimSpace(errorMessage),
	}
	ctx, cancel := context.WithTimeout(context.Background(), repoTimeout)
	defer cancel()
	delivered, err := s.repo.Deliver(ctx, state, canonicalProvider, payload)
	if err != nil {
		return err
	}
	if !delivered {
		return ErrNotPending
	}
	s.announce(state)
	return nil
}

// WaitCallback waits for the callback of a session this node started. Events
// wake it as soon as any node delivers the callback; the poll interval only
// bounds how long a lost event can delay it. pollInterval is ignored: it
// suits the memory store's file polling, not database reads.
func (s *ClusterStore) WaitCallback(_ string, provider, state string, timeout, _ time.Duration) (map[string]string, error) {
	canonicalProvider, err := NormalizeProvider(provider)
	if err != nil {
		return nil, err
	}
	if err := ValidateState(state); err != nil {
		return nil, err
	}
	state = strings.TrimSpace(state)
	if timeout <= 0 {
		timeout = DefaultTTL
	}
	s.ensureSubscribed()
	wake, release := s.addWaiter(state)
	defer release()

	deadline := time.Now().Add(timeout)
	for {
		payload, done, err := s.checkCallback(state, canonicalProvider)
		if err != nil {
			return nil, err
		}
		if done {
			return payload, nil
		}
		if time.Now().After(deadline) {
			return nil, ErrCallbackTimeout
		}
		wait := s.currentPollInterval()
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining + time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// checkCallback reads the session once. A read failure is retried on the next
// round rather than returned: giving up would abandon a login the user may
// already have approved in the browser.
func (s *ClusterStore) checkCallback(state, provider string) (map[string]string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), repoTimeout)
	defer cancel()
	rec, ok, err := s.repo.Get(ctx, state)
	if err != nil {
		log.WithError(err).Debug("oauth: shared session read failed while waiting for callback")
		return nil, false, nil
	}
	if !ok || rec.Status != RecordPending || rec.Expired || !strings.EqualFold(rec.Provider, provider) {
		return nil, false, ErrNotPending
	}
	payload, taken, err := s.repo.TakeCallback(ctx, state)
	if err != nil {
		log.WithError(err).Debug("oauth: shared session callback read failed")
		return nil, false, nil
	}
	return payload, taken, nil
}

func (s *ClusterStore) currentPollInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pollInterval
}

// announce wakes a waiter on this node and tells the other nodes. Publish
// skips the publisher itself, hence the local wake-up.
func (s *ClusterStore) announce(state string) {
	s.wake(state)
	ctx, cancel := context.WithTimeout(context.Background(), repoTimeout)
	defer cancel()
	if err := s.coord().Publish(ctx, cluster.TopicOAuth, cluster.OAuthEvent{State: state}); err != nil {
		log.WithError(err).Debug("oauth: failed to announce shared session change; the owner falls back to polling")
	}
}

func (s *ClusterStore) ensureSubscribed() {
	c := s.coord()
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil || c == s.subscribedTo {
		return
	}
	if s.unsubscribe != nil {
		s.unsubscribe()
	}
	s.subscribedTo = c
	s.unsubscribe = c.Subscribe(cluster.TopicOAuth, s.onEvent)
}

// onEvent runs on the bus goroutine and must not block.
func (s *ClusterStore) onEvent(ev cluster.Event) {
	if ev.Resync {
		// Events may have been lost while disconnected; every waiter re-reads.
		s.mu.Lock()
		for _, w := range s.waiters {
			signal(w.wake)
		}
		s.mu.Unlock()
		return
	}
	var payload cluster.OAuthEvent
	if err := ev.Decode(&payload); err != nil || strings.TrimSpace(payload.State) == "" {
		return
	}
	s.wake(strings.TrimSpace(payload.State))
}

func (s *ClusterStore) wake(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.waiters[state]; w != nil {
		signal(w.wake)
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *ClusterStore) addWaiter(state string) (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waiters[state]
	if w == nil {
		w = &waiter{wake: make(chan struct{}, 1)}
		s.waiters[state] = w
	}
	w.refs++
	return w.wake, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.refs--
		if w.refs <= 0 && s.waiters[state] == w {
			delete(s.waiters, state)
		}
	}
}

func (s *ClusterStore) startHeartbeat(state, owner string) {
	ctx, cancel := context.WithCancel(context.Background())
	beat := &heartbeat{cancel: cancel}
	s.mu.Lock()
	if previous := s.heartbeats[state]; previous != nil {
		previous.cancel()
	}
	s.heartbeats[state] = beat
	interval := s.beatInterval
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			beatCtx, beatCancel := context.WithTimeout(ctx, repoTimeout)
			alive, err := s.repo.Heartbeat(beatCtx, state, owner)
			beatCancel()
			if err != nil {
				log.WithError(err).Debug("oauth: shared login heartbeat failed")
				continue
			}
			if !alive {
				s.dropHeartbeat(state, beat)
				return
			}
		}
	}()
}

func (s *ClusterStore) stopHeartbeat(state string) {
	s.mu.Lock()
	beat := s.heartbeats[state]
	delete(s.heartbeats, state)
	s.mu.Unlock()
	if beat != nil {
		beat.cancel()
	}
}

// dropHeartbeat removes beat from state only if it is still the registered
// one, so a state registered again keeps its new heartbeat.
func (s *ClusterStore) dropHeartbeat(state string, beat *heartbeat) {
	s.mu.Lock()
	if s.heartbeats[state] == beat {
		delete(s.heartbeats, state)
	}
	s.mu.Unlock()
	beat.cancel()
}
