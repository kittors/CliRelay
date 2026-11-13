package accessfreeze

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// refreshInterval is how often a process rereads the standing freezes to
	// learn what other instances froze or released. This process's own changes
	// apply at once.
	refreshInterval = 30 * time.Second
	// jobTimeout bounds the database work for one refusal.
	jobTimeout = 10 * time.Second
	// jobQueue holds refusals waiting for the worker. Refusals are rare (32 in
	// four days on relay in November 2026); a full queue hands the job to a
	// goroutine of its own rather than drop it, since a dropped refusal is a
	// requester that should have been frozen and was not.
	jobQueue = 256
)

// Service records upstream content refusals, freezes the requesters that
// reach their tenant's threshold, and answers the request path from memory.
type Service struct {
	store    *Store
	registry *registry
	now      func() time.Time

	// transitions serializes the database decision and its cache publication.
	// The request path uses registry's own read lock, never this gate.
	transitions chan struct{}

	jobs      chan refusalJob
	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type refusalJob struct {
	subject   Subject
	event     coreauth.UpstreamRefusalEvent
	requestID string
	at        time.Time
}

// NewService returns a service over store; notices are written in loc.
func NewService(store *Store, loc *time.Location) *Service {
	return &Service{
		store:       store,
		registry:    newRegistry(loc),
		now:         time.Now,
		transitions: make(chan struct{}, 1),
		jobs:        make(chan refusalJob, jobQueue),
		stop:        make(chan struct{}),
	}
}

// Start loads the standing freezes, then keeps them current until Close.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.refresh(ctx)
	s.wg.Add(2)
	go s.work()
	go s.refreshLoop()
}

// Close stops the worker after the refusals already queued are recorded.
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *Service) refreshLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
			s.refresh(ctx)
			cancel()
		}
	}
}

// lockTransition waits without outliving the caller's deadline. A database
// result must be published before another local transition or refresh begins:
// otherwise an old active row can undo a completed release in the registry.
// This orders this process only; peers still learn changes through refresh.
func (s *Service) lockTransition(ctx context.Context) error {
	select {
	case s.transitions <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlockTransition()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) unlockTransition() { <-s.transitions }

func (s *Service) refresh(ctx context.Context) {
	if err := s.lockTransition(ctx); err != nil {
		return
	}
	defer s.unlockTransition()
	generation := s.registry.generation()
	freezes, err := s.store.ListActive(ctx)
	if err != nil {
		// Keep what is known: a database hiccup must not lift every freeze.
		log.Warnf("access freeze: reading standing freezes failed: %v", err)
		return
	}
	// Skipped when this process froze or released something during the read;
	// the next refresh installs a load that includes it.
	s.registry.replaceAllUnlessChanged(generation, freezes)
}

func (s *Service) work() {
	defer s.wg.Done()
	for {
		select {
		case job := <-s.jobs:
			s.handle(job)
		case <-s.stop:
			for {
				select {
				case job := <-s.jobs:
					s.handle(job)
				default:
					return
				}
			}
		}
	}
}

// OnUpstreamContentRefusal implements coreauth.UpstreamRefusalObserver. It
// runs on the request's goroutine, so it only reads who made the request and
// leaves the database work to the worker.
func (s *Service) OnUpstreamContentRefusal(ctx context.Context, event coreauth.UpstreamRefusalEvent) {
	if s == nil {
		return
	}
	subject, ok := subjectFromContext(ctx)
	if !ok {
		return
	}
	job := refusalJob{subject: subject, event: event, requestID: logging.GetRequestID(ctx), at: s.now()}
	select {
	case s.jobs <- job:
	default:
		go s.handle(job)
	}
}

func (s *Service) handle(job refusalJob) {
	// A queued refusal has already been admitted. Waiting for another state
	// transition must not consume its database-work deadline or drop evidence.
	// The existing worker queue already waits this way; overflow goroutines
	// retain their existing independent lifecycle.
	_ = s.lockTransition(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()
	var newlyFrozen *Freeze
	defer func() {
		s.unlockTransition()
		// Auditing is not part of cache ordering and may call other services.
		if newlyFrozen != nil {
			auditFreeze(ctx, *newlyFrozen)
		}
	}()
	strike := Strike{
		Subject:         job.subject,
		Provider:        normalizeProvider(job.event.Provider),
		ReasonCode:      job.event.Refusal.Code,
		UpstreamMessage: job.event.Refusal.Message,
		Model:           job.event.Model,
		AuthLabel:       job.event.AuthLabel,
		RequestID:       job.requestID,
		CreatedAt:       job.at,
	}
	if strike.Provider == "" {
		return
	}
	count, err := s.store.RecordStrike(ctx, strike)
	if err != nil {
		log.Errorf("access freeze: recording the %s refusal of %s %s failed: %v", strike.ReasonCode, strike.Subject.Kind, strike.Subject.ID, err)
		return
	}
	// A refusal of a request that was already in flight when the freeze came.
	if standing, ok, errStanding := s.store.CountOnStanding(ctx, strike.Subject, strike.Provider); errStanding != nil {
		log.Errorf("access freeze: counting a refusal on the standing %s freeze of %s %s failed: %v", strike.Provider, strike.Subject.Kind, strike.Subject.ID, errStanding)
		return
	} else if ok {
		s.registry.put(standing)
		return
	}
	threshold, err := s.store.Threshold(ctx, strike.Subject.TenantID)
	if err != nil {
		// Freezing on the default is the safer reading: a missed freeze lets the
		// requester keep sending the content to the pool's accounts.
		log.Warnf("access freeze: reading the threshold of tenant %s failed, using %d: %v", strike.Subject.TenantID, DefaultThreshold, err)
		threshold = DefaultThreshold
	}
	if threshold <= 0 || count < threshold {
		log.Infof("access freeze: %s refused a %s request of %s %s for its content (%s), %d of %d", strike.Provider, strike.Model, strike.Subject.Kind, strike.Subject.ID, strike.ReasonCode, count, threshold)
		return
	}
	freeze, created, err := s.store.Freeze(ctx, strike, count, s.now())
	if err != nil {
		log.Errorf("access freeze: freezing %s for %s %s failed: %v", strike.Provider, strike.Subject.Kind, strike.Subject.ID, err)
		return
	}
	s.registry.put(freeze)
	if created {
		log.Warnf("access freeze: froze %s for %s %s of tenant %s after %d refusal(s) (%s, model %s, request %s)",
			freeze.Provider, freeze.Subject.Kind, freeze.Subject.ID, freeze.Subject.TenantID, freeze.StrikeCount, freeze.ReasonCode, freeze.Model, freeze.RequestID)
		newlyFrozen = &freeze
	}
}

// auditFreeze records an automatic freeze in the audit log the way the login
// lockout records its cooldowns: the actor is the system. A release needs no
// line of its own here; it is a management write, which the management
// middleware audits.
func auditFreeze(ctx context.Context, f Freeze) {
	service := identity.Default()
	if service == nil {
		return
	}
	resourceType := "end_user"
	if f.Subject.Kind == SubjectAPIKey {
		resourceType = "api_key"
	}
	service.RecordAudit(ctx, identity.AuditEvent{
		TenantID:     f.Subject.TenantID,
		ActorKind:    "system",
		Action:       "access_freeze.freeze",
		ResourceType: resourceType,
		ResourceID:   f.Subject.ID,
		Result:       "success",
		RequestID:    f.RequestID,
		Changes: map[string]any{
			"freeze_id":    f.ID,
			"provider":     f.Provider,
			"reason_code":  f.ReasonCode,
			"model":        f.Model,
			"strike_count": f.StrikeCount,
		},
	})
}

// FrozenProviders is the requester's frozen providers as the access metadata
// carries them (cliproxyexecutor.AccessFrozenProvidersKey), or "".
func (s *Service) FrozenProviders(tenantID, endUserID, apiKeyID string) string {
	if s == nil {
		return ""
	}
	subject, ok := subjectFromAccess(tenantID, endUserID, apiKeyID)
	if !ok {
		return ""
	}
	return s.registry.frozenProviders(subject)
}

// Standing returns the subject's standing freezes, oldest first.
func (s *Service) Standing(subject Subject) []Freeze {
	if s == nil {
		return nil
	}
	return s.registry.list(subject)
}

// NoticeOf is the notice the requester of f reads.
func (s *Service) NoticeOf(f Freeze) string {
	return Notice(f, s.registry.loc)
}

// Store is the service's store.
func (s *Service) Store() *Store {
	return s.store
}

// List returns a tenant's freezes, newest first.
func (s *Service) List(ctx context.Context, tenantID string, filter Filter) ([]Freeze, error) {
	return s.store.List(ctx, tenantID, filter)
}

// ListStrikes returns a subject's refusals, newest first.
func (s *Service) ListStrikes(ctx context.Context, subject Subject, provider string, limit int) ([]Strike, error) {
	return s.store.ListStrikes(ctx, subject, provider, limit)
}

// Release lifts one of the tenant's freezes; the requester can use the
// provider again on its next request to this process.
func (s *Service) Release(ctx context.Context, tenantID string, id int64, by, note string) (Freeze, error) {
	// Bound both waiting and database work, even when the HTTP client supplied
	// no deadline; a stuck management write must not hold the worker forever.
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	if err := s.lockTransition(ctx); err != nil {
		return Freeze{}, err
	}
	defer s.unlockTransition()
	freeze, err := s.store.Release(ctx, tenantID, id, strings.TrimSpace(by), strings.TrimSpace(note), s.now())
	if err != nil {
		return Freeze{}, err
	}
	s.registry.put(freeze)
	log.Infof("access freeze: %s released the %s freeze %d of %s %s", freeze.ReleasedBy, freeze.Provider, freeze.ID, freeze.Subject.Kind, freeze.Subject.ID)
	return freeze, nil
}

// Threshold is the tenant's threshold (0: record refusals, never freeze).
func (s *Service) Threshold(ctx context.Context, tenantID string) (int, error) {
	return s.store.Threshold(ctx, tenantID)
}

// SetThreshold stores the tenant's threshold.
func (s *Service) SetThreshold(ctx context.Context, tenantID string, threshold int, by string) error {
	return s.store.SetThreshold(ctx, tenantID, threshold, strings.TrimSpace(by), s.now())
}

var installed atomic.Pointer[Service]

// SetRuntime installs the process's service for the request path and the
// management API.
func SetRuntime(s *Service) {
	installed.Store(s)
}

// Runtime returns the installed service, or nil.
func Runtime() *Service {
	return installed.Load()
}

// FrozenProvidersFor is FrozenProviders of the installed service; "" when
// none is installed.
func FrozenProvidersFor(tenantID, endUserID, apiKeyID string) string {
	return Runtime().FrozenProviders(tenantID, endUserID, apiKeyID)
}

// NoticeFor is the notice the requester of f reads, in the installed
// service's time zone, or the local one when none is installed.
func NoticeFor(f Freeze) string {
	if service := Runtime(); service != nil {
		return service.NoticeOf(f)
	}
	return Notice(f, time.Local)
}

// Release lifts one of the tenant's freezes in store. When this process runs
// the service, the release goes through it so the request path stops holding
// the requester back at once; other processes see it on their next refresh.
func Release(ctx context.Context, store *Store, tenantID string, id int64, by, note string) (Freeze, error) {
	if service := Runtime(); service != nil {
		return service.Release(ctx, tenantID, id, by, note)
	}
	return store.Release(ctx, tenantID, id, strings.TrimSpace(by), strings.TrimSpace(note), time.Now())
}
