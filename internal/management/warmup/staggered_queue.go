package warmup

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Task represents an individual account warmup job in the queue.
type Task struct {
	ID        string
	TenantID  string
	Auth      *coreauth.Auth
	Target    Target
	Delay     time.Duration
	CreatedAt time.Time
}

// StaggeredQueueOptions configures the rate-limiting and anti-abuse stagger parameters.
type StaggeredQueueOptions struct {
	MaxConcurrency int           // Maximum concurrent executions (default: 2)
	MinJitter      time.Duration // Minimum jitter delay between account tasks (default: 1s)
	MaxJitter      time.Duration // Maximum jitter delay between account tasks (default: 5s)
}

// QueueMetrics tracks execution stats.
type QueueMetrics struct {
	TotalQueued    int64
	TotalExecuted  int64
	TotalSucceeded int64
	TotalFailed    int64
	InFlightCount  int64
}

// StaggeredQueue manages smooth, staggered dispatching of warmup tasks.
type StaggeredQueue struct {
	opts     StaggeredQueueOptions
	registry *DriverRegistry
	sem      chan struct{}

	mu         sync.Mutex
	activeAuth map[string]struct{} // single-account mutual exclusion

	metrics QueueMetrics

	onResult func(result *Result)
}

func NewStaggeredQueue(opts StaggeredQueueOptions, registry *DriverRegistry, onResult func(result *Result)) *StaggeredQueue {
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 2
	}
	if opts.MinJitter <= 0 {
		opts.MinJitter = 1 * time.Second
	}
	if opts.MaxJitter < opts.MinJitter {
		opts.MaxJitter = opts.MinJitter + 2*time.Second
	}

	return &StaggeredQueue{
		opts:       opts,
		registry:   registry,
		sem:        make(chan struct{}, opts.MaxConcurrency),
		activeAuth: make(map[string]struct{}),
		onResult:   onResult,
	}
}

// Dispatch spreads the given batch of accounts across a target duration with random jitter.
func (q *StaggeredQueue) Dispatch(ctx context.Context, tasks []Task) {
	atomic.AddInt64(&q.metrics.TotalQueued, int64(len(tasks)))

	go func() {
		for i, task := range tasks {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// Add task delay if configured
			if task.Delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(task.Delay):
				}
			}

			// Apply random jitter between item dispatches to avoid pattern detection
			jitter := q.randomJitter()
			if i > 0 && jitter > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(jitter):
				}
			}

			q.executeTaskAsync(ctx, task)
		}
	}()
}

func (q *StaggeredQueue) executeTaskAsync(ctx context.Context, task Task) {
	// Account-level mutual exclusion
	q.mu.Lock()
	if _, busy := q.activeAuth[task.Auth.ID]; busy {
		q.mu.Unlock()
		log.Debugf("warmup: skipping task for %s, already active", task.Auth.ID)
		return
	}
	q.activeAuth[task.Auth.ID] = struct{}{}
	q.mu.Unlock()

	go func() {
		defer func() {
			q.mu.Lock()
			delete(q.activeAuth, task.Auth.ID)
			q.mu.Unlock()
		}()

		// Acquire global concurrency slot
		select {
		case <-ctx.Done():
			return
		case q.sem <- struct{}{}:
		}
		defer func() { <-q.sem }()

		atomic.AddInt64(&q.metrics.InFlightCount, 1)
		defer atomic.AddInt64(&q.metrics.InFlightCount, -1)

		driver, ok := q.registry.Get(task.Auth.Provider)
		if !ok {
			log.Warnf("warmup: no driver for provider %s", task.Auth.Provider)
			atomic.AddInt64(&q.metrics.TotalFailed, 1)
			return
		}

		res, err := driver.ExecuteWarmup(ctx, task.Auth, task.Target)
		atomic.AddInt64(&q.metrics.TotalExecuted, 1)

		if err != nil || (res != nil && !res.Success) {
			atomic.AddInt64(&q.metrics.TotalFailed, 1)
		} else {
			atomic.AddInt64(&q.metrics.TotalSucceeded, 1)
		}

		if q.onResult != nil && res != nil {
			q.onResult(res)
		}
	}()
}

func (q *StaggeredQueue) randomJitter() time.Duration {
	diff := q.opts.MaxJitter - q.opts.MinJitter
	if diff <= 0 {
		return q.opts.MinJitter
	}
	return q.opts.MinJitter + time.Duration(rand.Int63n(int64(diff)))
}

func (q *StaggeredQueue) GetMetrics() QueueMetrics {
	return QueueMetrics{
		TotalQueued:    atomic.LoadInt64(&q.metrics.TotalQueued),
		TotalExecuted:  atomic.LoadInt64(&q.metrics.TotalExecuted),
		TotalSucceeded: atomic.LoadInt64(&q.metrics.TotalSucceeded),
		TotalFailed:    atomic.LoadInt64(&q.metrics.TotalFailed),
		InFlightCount:  atomic.LoadInt64(&q.metrics.InFlightCount),
	}
}
