package usage

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	Provider            string
	Model               string
	ThinkingLevel       string
	UpstreamModel       string
	VisionFallbackModel string
	// UpstreamResponseModel is the model the upstream declared in its own
	// response body, empty when it declared none. It is recorded for auditing
	// only and never substitutes for Model in logging or cost.
	UpstreamResponseModel string
	APIKey                string
	APIKeyID              string
	APIKeyName            string
	AuthID                string
	AuthIndex             string
	AuthSubjectID         string
	Source                string
	ChannelName           string
	RequestedAt           time.Time
	LatencyMs             int64
	FirstTokenMs          int64
	Failed                bool
	APIIdentifier         string
	RequestID             string
	ResponseStatus        int
	Streaming             bool
	Detail                Detail

	// TrustedTenantID is set only by authenticated internal execution paths.
	// When empty, persistence resolves the tenant from the real API key.
	TrustedTenantID string

	// IdempotencyKey identifies this record for exactly-once persistence.
	// Publish assigns one when it is empty, so a write that is retried or
	// replayed after the database acknowledged it can be recognised and
	// skipped instead of being counted twice.
	IdempotencyKey string

	// Optional: request/response content for log detail viewer.
	// These are stored in the database when non-empty and can be retrieved via the
	// /usage/logs/:id/content API. The persistence layer may compress and retain
	// content according to runtime configuration.
	InputContent  string
	OutputContent string
	DetailContent string

	// Optional temp-file backed content. Large request/response bodies use these
	// paths so the bounded manager queue retains small references instead of
	// whole payload strings. Plugins must consume them synchronously; Manager
	// removes the files after dispatch.
	InputContentPath  string
	OutputContentPath string
	DetailContentPath string
}

// Detail holds the token usage breakdown.
type Detail struct {
	InputTokens              int64
	OutputTokens             int64
	ReasoningTokens          int64
	CachedTokens             int64 // legacy/compat: equals CacheReadTokens if cache read exists, else equals CacheWriteTokens
	TotalTokens              int64
	CacheReadTokens          int64 // tokens served from cache (cache read / cache hit)
	CacheWriteTokens         int64 // tokens written to cache (cache creation)
	CacheReadIncludedInInput bool  // when true, CacheReadTokens is a subset of InputTokens (OpenAI-compatible style)
}

// Plugin consumes usage records emitted by the proxy runtime.
type Plugin interface {
	HandleUsage(ctx context.Context, record Record)
}

// OverflowHandler takes a record the bounded queue could not accept, either
// because it is full or because the manager has stopped. It runs on the
// publishing goroutine, which is usually still serving the client, so it must
// finish quickly and never wait on the database. It reports whether the record
// was kept; a false return means the record is lost.
type OverflowHandler func(Record) bool

type queueItem struct {
	record Record
}

// Manager maintains a queue of usage records and delivers them to registered plugins.
type Manager struct {
	once     sync.Once
	stopOnce sync.Once
	cancel   context.CancelFunc
	started  atomic.Bool
	done     chan struct{}

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []queueItem
	capacity int
	closed   bool

	overflow atomic.Pointer[OverflowHandler]

	pluginsMu sync.RWMutex
	plugins   []Plugin
}

// NewManager constructs a manager with a buffered queue.
func NewManager(buffer int) *Manager {
	if buffer <= 0 {
		buffer = 1
	}
	m := &Manager{capacity: buffer, done: make(chan struct{})}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// NewIdempotencyKey returns a fresh record key. Keys are UUIDv7 so they sort
// by creation time, which keeps the database's unique index append-mostly.
func NewIdempotencyKey() string {
	if key, err := uuid.NewV7(); err == nil {
		return key.String()
	}
	return uuid.NewString()
}

// SetOverflowHandler installs fn as the destination for records the queue
// cannot take. With a handler installed Publish never blocks; without one a
// full queue makes Publish wait for space, which is the original contract.
// Passing nil restores the blocking behaviour.
func (m *Manager) SetOverflowHandler(fn OverflowHandler) {
	if m == nil {
		return
	}
	if fn == nil {
		m.overflow.Store(nil)
	} else {
		m.overflow.Store(&fn)
	}
	// Wake publishers that are waiting for space so they re-check and hand
	// their record to the new handler instead.
	m.cond.Broadcast()
}

func (m *Manager) overflowHandler() OverflowHandler {
	if fn := m.overflow.Load(); fn != nil {
		return *fn
	}
	return nil
}

// Start launches the background dispatcher. Calling Start multiple times is safe.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.once.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		var workerCtx context.Context
		workerCtx, m.cancel = context.WithCancel(ctx)
		m.started.Store(true)
		go m.run(workerCtx)
	})
}

// Wait blocks until the dispatcher has delivered every queued record and
// exited after Stop, or until ctx is done. It reports whether the dispatcher
// finished. Shutdown uses it so records still queued are persisted (or
// spooled) before the database handle is closed underneath them.
func (m *Manager) Wait(ctx context.Context) bool {
	if m == nil || !m.started.Load() {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-m.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Pending returns how many records are queued and not yet dispatched.
func (m *Manager) Pending() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queue)
}

// Stop stops the dispatcher and drains the queue.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cond.Broadcast()
	})
}

// Register appends a plugin to the delivery list.
func (m *Manager) Register(plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	m.pluginsMu.Lock()
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// Publish enqueues a usage record for processing. If no plugin is registered
// the record will be discarded downstream. The original request context is not
// retained in the asynchronous queue; required request metadata belongs in Record.
//
// When the queue is full (or the manager has stopped) and an overflow handler
// is installed, the record goes to the handler on this goroutine instead of
// waiting: the publisher is typically a request that has not answered its
// client yet, and a stalled database must not hold it hostage.
func (m *Manager) Publish(ctx context.Context, record Record) {
	if m == nil {
		return
	}
	// ensure worker is running even if Start was not called explicitly
	m.Start(context.Background())
	if record.IdempotencyKey == "" {
		record.IdempotencyKey = NewIdempotencyKey()
	}
	m.mu.Lock()
	for !m.closed && len(m.queue) >= m.capacity && m.overflowHandler() == nil {
		m.cond.Wait()
	}
	if m.closed || len(m.queue) >= m.capacity {
		m.mu.Unlock()
		if overflow := m.overflowHandler(); overflow != nil {
			invokeOverflow(overflow, record)
		}
		cleanupRecordTempFiles(record)
		return
	}
	m.queue = append(m.queue, queueItem{record: record})
	m.mu.Unlock()
	m.cond.Signal()
}

func invokeOverflow(fn OverflowHandler, record Record) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: overflow handler panic recovered: %v", r)
		}
	}()
	fn(record)
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	for {
		m.mu.Lock()
		for !m.closed && len(m.queue) == 0 {
			m.cond.Wait()
		}
		if len(m.queue) == 0 && m.closed {
			m.mu.Unlock()
			return
		}
		item := m.queue[0]
		m.queue[0] = queueItem{}
		m.queue = m.queue[1:]
		m.cond.Broadcast()
		m.mu.Unlock()
		m.dispatch(item)
	}
}

func (m *Manager) dispatch(item queueItem) {
	defer cleanupRecordTempFiles(item.record)
	m.pluginsMu.RLock()
	plugins := make([]Plugin, len(m.plugins))
	copy(plugins, m.plugins)
	m.pluginsMu.RUnlock()
	if len(plugins) == 0 {
		return
	}
	for _, plugin := range plugins {
		if plugin == nil {
			continue
		}
		safeInvoke(plugin, context.Background(), item.record)
	}
}

func cleanupRecordTempFiles(record Record) {
	for _, path := range []string{record.InputContentPath, record.OutputContentPath, record.DetailContentPath} {
		if path == "" {
			continue
		}
		_ = os.Remove(path)
	}
}

func safeInvoke(plugin Plugin, ctx context.Context, record Record) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: plugin panic recovered: %v", r)
		}
	}()
	plugin.HandleUsage(ctx, record)
}

var defaultManager = NewManager(512)

// DefaultManager returns the global usage manager instance.
func DefaultManager() *Manager { return defaultManager }

// RegisterPlugin registers a plugin on the default manager.
func RegisterPlugin(plugin Plugin) { DefaultManager().Register(plugin) }

// PublishRecord publishes a record using the default manager.
func PublishRecord(ctx context.Context, record Record) { DefaultManager().Publish(ctx, record) }

// StartDefault starts the default manager's dispatcher.
func StartDefault(ctx context.Context) { DefaultManager().Start(ctx) }

// StopDefault stops the default manager's dispatcher.
func StopDefault() { DefaultManager().Stop() }

// SetOverflowHandler installs fn on the default manager; see Manager.SetOverflowHandler.
func SetOverflowHandler(fn OverflowHandler) { DefaultManager().SetOverflowHandler(fn) }

// WaitDefault waits for the default manager's dispatcher; see Manager.Wait.
func WaitDefault(ctx context.Context) bool { return DefaultManager().Wait(ctx) }

// PendingDefault returns the default manager's queue depth.
func PendingDefault() int { return DefaultManager().Pending() }
