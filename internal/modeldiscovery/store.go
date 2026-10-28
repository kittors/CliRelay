// Package modeldiscovery keeps the model lists upstream providers advertise for
// their credentials, per tenant and provider.
//
// Several providers publish the models a credential can call: the ChatGPT Codex
// manifest, Anthropic /v1/models, xAI and Kimi listings. One store serves two
// consumers. The management panels show the list, and credential registration
// merges it into what the request router can reach. Keeping both on the same store
// is what makes "the panel lists this model" and "a request can reach it" the same
// statement; before, Codex discovery fed the panels only, so a model ChatGPT had
// shipped showed up in the catalog and still failed with "no provider serves model".
//
// A stored list is kept until a newer answer replaces it, across restarts too (see
// persist.go). The freshness window only decides when to ask again: a routable
// model must not disappear because a timer ran out, because the upstream listing
// endpoint had a bad minute, or because the process restarted.
package modeldiscovery

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

// FreshFor is how long a stored list is served without asking the upstream again.
const FreshFor = 24 * time.Hour

// routingProviders are the providers whose discovered models become routable.
//
// Codex is here because the manifest is what the Codex CLI itself polls, so asking
// for it periodically looks like any client. Claude is deliberately absent even
// though its discovery works the same way: nothing has confirmed how Anthropic
// treats OAuth credentials that list models on a timer, and its accounts keep the
// compiled-in catalog until something does. xAI and Kimi register their live lists
// directly during registration and need no merge.
var routingProviders = map[string]struct{}{
	"codex": {},
}

// sharedProviders are the providers whose panels read one list per tenant rather
// than asking the upstream once per account.
var sharedProviders = map[string]struct{}{
	"claude": {},
	"codex":  {},
	"xai":    {},
	"kimi":   {},
}

// NormalizeProvider maps provider aliases onto the key lists are stored under. xAI
// accounts may appear as xai, x-ai or grok depending on auth metadata.
func NormalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "x-ai", "grok":
		return "xai"
	default:
		return provider
	}
}

// IsShared reports whether a provider's panels share one discovered list per tenant.
func IsShared(provider string) bool {
	_, ok := sharedProviders[NormalizeProvider(provider)]
	return ok
}

// DrivesRouting reports whether a provider's discovered models are merged into the
// models its credentials register.
func DrivesRouting(provider string) bool {
	_, ok := routingProviders[NormalizeProvider(provider)]
	return ok
}

// RoutingProviders lists the providers DrivesRouting accepts, sorted.
func RoutingProviders() []string {
	out := make([]string, 0, len(routingProviders))
	for provider := range routingProviders {
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}

// IsDiscoveryCredential reports whether a credential may speak for its provider's
// shared list.
//
// For Codex and Claude only OAuth credentials qualify. An API key usually points at
// a relay with its own base URL, and its models endpoint describes that relay's
// catalog; letting it into the shared list would make every OAuth account of the
// tenant advertise the relay's models.
func IsDiscoveryCredential(auth *coreauth.Auth, provider string) bool {
	if auth == nil {
		return false
	}
	switch NormalizeProvider(provider) {
	case "codex", "claude":
		return !isAPIKeyCredential(auth)
	default:
		return true
	}
}

func isAPIKeyCredential(auth *coreauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "apikey")
}

type entry struct {
	models    []*sdkmodelcatalog.ModelInfo
	signature string
	fetchedAt time.Time
}

type flight struct {
	done   chan struct{}
	models []*sdkmodelcatalog.ModelInfo
	ok     bool
}

var (
	mu            sync.Mutex
	entries       = map[string]entry{}
	inflight      = map[string]*flight{}
	routingChange func(tenantID, provider string)
	now           = time.Now
)

func storeKey(tenantID, provider string) string {
	return coreauth.NormalizedTenantID(tenantID) + "|" + NormalizeProvider(provider)
}

// SetRoutingChangeHook registers the callback run after a routing provider's list
// changes for a tenant. Registration uses it to re-register that tenant's
// credentials; nil removes it.
func SetRoutingChangeHook(fn func(tenantID, provider string)) {
	mu.Lock()
	routingChange = fn
	mu.Unlock()
}

// Fresh returns the stored list when it is younger than FreshFor.
func Fresh(tenantID, provider string) []*sdkmodelcatalog.ModelInfo {
	if !IsShared(provider) {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	stored, ok := entries[storeKey(tenantID, provider)]
	if !ok || now().Sub(stored.fetchedAt) > FreshFor {
		return nil
	}
	return cloneModels(stored.models)
}

// Listed reports whether provider's stored list for the tenant contains modelID.
// A channel group that follows upstream (the root group unless an operator
// changed it) uses the Codex list so a model Codex publishes is reachable
// without an operator appending it to an allow list. Other providers stay
// behind that list: an xAI or Kimi id discovery happens to return is not a
// model the group was asked to grow. Exclusions are the caller's job.
func Listed(tenantID, provider, modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	provider = NormalizeProvider(provider)
	if modelID == "" || provider == "" || !IsShared(provider) {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	entry, ok := entries[storeKey(tenantID, provider)]
	if !ok {
		return false
	}
	for _, model := range entry.models {
		if model != nil && strings.EqualFold(strings.TrimSpace(model.ID), modelID) {
			return true
		}
	}
	return false
}

// Snapshot returns the last list stored for a tenant and provider, however old.
func Snapshot(tenantID, provider string) []*sdkmodelcatalog.ModelInfo {
	if !IsShared(provider) {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	return cloneModels(entries[storeKey(tenantID, provider)].models)
}

// Store records a live answer. An empty answer is ignored: "the upstream listed
// nothing" is indistinguishable from a broken response, and acting on it would
// deregister every discovered model at once.
func Store(tenantID, provider string, models []*sdkmodelcatalog.ModelInfo) {
	provider = NormalizeProvider(provider)
	if !IsShared(provider) {
		return
	}
	cloned := cloneModels(models)
	if len(cloned) == 0 {
		return
	}
	tenantID = coreauth.NormalizedTenantID(tenantID)
	signature := listSignature(cloned)

	mu.Lock()
	key := storeKey(tenantID, provider)
	previous := entries[key]
	entries[key] = entry{models: cloned, signature: signature, fetchedAt: now()}
	hook := routingChange
	mu.Unlock()

	// Written before the hook re-registers, so a restart right after it still
	// routes this list.
	if err := persist(); err != nil {
		log.Warnf("model discovery: saving the provider model lists failed: %v", err)
	}

	if hook != nil && DrivesRouting(provider) && previous.signature != signature {
		hook(tenantID, provider)
	}
}

// listSignature captures what registration derives from a list, so a re-fetch that
// returns the same models does not re-register every credential.
func listSignature(models []*sdkmodelcatalog.ModelInfo) string {
	parts := make([]string, 0, len(models))
	for _, model := range models {
		var levels string
		if model.Thinking != nil {
			levels = strings.Join(model.Thinking.Levels, ",")
		}
		parts = append(parts, strings.Join([]string{
			strings.ToLower(strings.TrimSpace(model.ID)),
			levels,
			strconv.Itoa(model.ContextLength),
			strconv.FormatBool(model.UserDefined),
		}, "|"))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func cloneModels(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		clone := *model
		clone.SupportedGenerationMethods = append([]string(nil), model.SupportedGenerationMethods...)
		clone.SupportedParameters = append([]string(nil), model.SupportedParameters...)
		if model.Thinking != nil {
			thinking := *model.Thinking
			thinking.Levels = append([]string(nil), model.Thinking.Levels...)
			clone.Thinking = &thinking
		}
		out = append(out, &clone)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SetClockForTest makes the store read time from clock and returns a func restoring
// the real clock. clock must not call into this package.
func SetClockForTest(clock func() time.Time) func() {
	mu.Lock()
	previous := now
	now = clock
	mu.Unlock()
	return func() {
		mu.Lock()
		now = previous
		mu.Unlock()
	}
}

// ResetForTest clears every stored list and in-flight fetch and turns persistence
// off. The hook is left alone.
func ResetForTest() {
	mu.Lock()
	entries = map[string]entry{}
	inflight = map[string]*flight{}
	snapshotPath = ""
	mu.Unlock()
}
