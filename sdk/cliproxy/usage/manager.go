package usage

import (
	"context"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	Provider            string
	Model               string
	UpstreamModel       string
	VisionFallbackModel string
	APIKey              string
	APIKeyID            string
	APIKeyName          string
	AuthID              string
	AuthIndex           string
	AuthSubjectID       string
	Source              string
	ChannelName         string
	RequestedAt         time.Time
	LatencyMs           int64
	FirstTokenMs        int64
	Failed              bool
	APIIdentifier       string
	RequestID           string
	ResponseStatus      int
	Streaming           bool
	Detail              Detail

	// Optional: request/response content for log detail viewer.
	// These are stored in SQLite when non-empty and can be retrieved via the
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

type queueItem struct {
	record Record
}

// Manager maintains a queue of usage records and delivers them to registered plugins.
type Manager struct {
	once     sync.Once
	stopOnce sync.Once
	cancel   context.CancelFunc

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []queueItem
	capacity int
	closed   bool

	pluginsMu sync.RWMutex
	plugins   []Plugin
}

// NewManager constructs a manager with a buffered queue.
func NewManager(buffer int) *Manager {
	if buffer <= 0 {
		buffer = 1
	}
	m := &Manager{capacity: buffer}
	m.cond = sync.NewCond(&m.mu)
	return m
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
		go m.run(workerCtx)
	})
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
func (m *Manager) Publish(ctx context.Context, record Record) {
	if m == nil {
		return
	}
	// ensure worker is running even if Start was not called explicitly
	m.Start(context.Background())
	m.mu.Lock()
	for !m.closed && len(m.queue) >= m.capacity {
		m.cond.Wait()
	}
	if m.closed {
		m.mu.Unlock()
		cleanupRecordTempFiles(record)
		return
	}
	m.queue = append(m.queue, queueItem{record: record})
	m.mu.Unlock()
	m.cond.Signal()
}

func (m *Manager) run(ctx context.Context) {
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
