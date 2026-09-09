package config

import "time"

// DefaultAccountConcurrencyWaitSeconds is how long a request waits for a busy
// account when the config omits the field. Queuing is the default because a
// per-account limit is meant to pace traffic, not to reject it: without a wait
// the request after the limit fails even though the account frees up moments later.
const DefaultAccountConcurrencyWaitSeconds = 30

// AccountConcurrencyConfig controls what happens once an AI account reaches the
// `concurrency_limit` recorded on its auth file.
type AccountConcurrencyConfig struct {
	// WaitTimeoutSeconds is how long a request may queue for a saturated account
	// before giving up with a 429. Omit it for the default; set 0 to disable
	// queuing and fail as soon as every candidate account is busy.
	WaitTimeoutSeconds *int `yaml:"wait-timeout-seconds,omitempty" json:"wait-timeout-seconds,omitempty"`

	// MaxQueueDepth caps how many requests may queue per account. 0 means
	// unbounded; set it when a stalled upstream should shed load instead of
	// letting callers pile up behind it.
	MaxQueueDepth int `yaml:"max-queue-depth,omitempty" json:"max-queue-depth,omitempty"`
}

// WaitTimeout resolves the configured queue timeout. A zero duration disables queuing.
func (c AccountConcurrencyConfig) WaitTimeout() time.Duration {
	seconds := DefaultAccountConcurrencyWaitSeconds
	if c.WaitTimeoutSeconds != nil {
		seconds = *c.WaitTimeoutSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// QueueDepth resolves the per-account queue cap, treating negatives as unbounded.
func (c AccountConcurrencyConfig) QueueDepth() int {
	if c.MaxQueueDepth < 0 {
		return 0
	}
	return c.MaxQueueDepth
}
