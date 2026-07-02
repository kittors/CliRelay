package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

const (
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshMaxConcurrency = 16
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	quotaBackoffBase      = time.Second
	quotaBackoffMax       = 30 * time.Minute
)

// PermanentAuthError indicates a refresh failure that should be surfaced as
// unavailable instead of retried immediately. It is not proof that the user
// intentionally removed the credential.
type PermanentAuthError struct {
	Reason string
	Cause  error
}

func (e *PermanentAuthError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("permanent auth failure: %s: %v", e.Reason, e.Cause)
	}
	return fmt.Sprintf("permanent auth failure: %s", e.Reason)
}

func (e *PermanentAuthError) Unwrap() error { return e.Cause }

// IsPermanentAuthError reports whether err (or any error in its chain)
// is a PermanentAuthError.
func IsPermanentAuthError(err error) bool {
	var p *PermanentAuthError
	return errors.As(err, &p)
}

var quotaCooldownDisabled atomic.Bool

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Headers carries upstream response headers used for provider-specific
	// runtime health decisions such as Anthropic rate-limit windows.
	Headers http.Header
	// Error describes the failure when Success is false.
	Error *Error
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store                 Store
	executors             map[string]ProviderExecutor
	selector              Selector
	roundRobinSelector    *RoundRobinSelector
	fillFirstSelector     *FillFirstSelector
	sessionStickySelector *SessionStickySelector
	hook                  Hook
	mu                    sync.RWMutex
	auths                 map[string]*Auth
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int

	// Retry controls request retry behavior.
	requestRetry     atomic.Int32
	maxRetryInterval atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// runtimeConfig stores the narrowed runtime config snapshot used for
	// request-time routing and alias resolution.
	runtimeConfig atomic.Value

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	modelRegistry ModelRegistry

	// Auto refresh state
	refreshCancel    context.CancelFunc
	refreshSemaphore chan struct{}
	quotaProbeAfter  map[string]time.Time
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	roundRobinSelector, _ := selector.(*RoundRobinSelector)
	if roundRobinSelector == nil {
		roundRobinSelector = &RoundRobinSelector{}
	}
	fillFirstSelector, _ := selector.(*FillFirstSelector)
	if fillFirstSelector == nil {
		fillFirstSelector = &FillFirstSelector{}
	}
	sessionStickySelector, _ := selector.(*SessionStickySelector)
	if sessionStickySelector == nil {
		sessionStickySelector = NewSessionStickySelector(roundRobinSelector)
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:                 store,
		executors:             make(map[string]ProviderExecutor),
		selector:              selector,
		roundRobinSelector:    roundRobinSelector,
		fillFirstSelector:     fillFirstSelector,
		sessionStickySelector: sessionStickySelector,
		hook:                  hook,
		auths:                 make(map[string]*Auth),
		providerOffsets:       make(map[string]int),
		refreshSemaphore:      make(chan struct{}, refreshMaxConcurrency),
		quotaProbeAfter:       make(map[string]time.Time),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(newRuntimeConfigSnapshot(nil))
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	AttachDefaultModelRegistry(manager)
	return manager
}

func (m *Manager) SetModelRegistry(registry ModelRegistry) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.modelRegistry = registry
	m.mu.Unlock()
}
