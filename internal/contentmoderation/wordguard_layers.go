package contentmoderation

import (
	"strings"
	"sync"
)

// wordContext is what applies together to one request: a set of rules and the
// settings that say how to scan for them and what a hit does.
//
// A request has at most two. The platform's own context always comes first and
// nothing below it can touch it, because letting a tenant or a key change the
// platform's action or scope would be the same as letting them switch it off.
// The tenant's context follows, with the API key's adjustments folded in.
type wordContext struct {
	// layer is the highest layer that contributed rules to the context.
	layer        string
	action       string
	replacement  string
	scanScope    string
	blockMessage string
	// fold is the Traditional-to-Simplified setting for matching.
	fold bool
	// reply says the model's answer is filtered with this context as well.
	reply   bool
	matcher *wordMatcher
}

// wordSettings is the result of merging the settings of the policies that
// make up one context. The later policy wins wherever it sets something.
type wordSettings struct {
	action, replacement, scanScope, blockMessage string
	fold, reply                                  bool
}

func mergeWordSettings(policies ...*WordPolicy) wordSettings {
	settings := wordSettings{
		action:       WordActionBlock,
		replacement:  DefaultWordReplacement,
		scanScope:    WordScanAllUser,
		blockMessage: DefaultBlockMessage,
	}
	for _, p := range policies {
		if p == nil {
			continue
		}
		if p.Action != "" {
			settings.action = p.Action
		}
		if p.Replacement != "" {
			settings.replacement = p.Replacement
		}
		if p.ScanScope != "" {
			settings.scanScope = p.ScanScope
		}
		if strings.TrimSpace(p.BlockMessage) != "" {
			settings.blockMessage = p.BlockMessage
		}
		if p.FoldTraditionalChinese != nil {
			settings.fold = *p.FoldTraditionalChinese
		}
		if p.ReplyFilter != nil {
			settings.reply = *p.ReplyFilter
		}
	}
	return settings
}

// ruleRefs lists a policy's enabled rules tagged with the layer they belong to.
func ruleRefs(p *WordPolicy, layer string) []wordRuleRef {
	if p == nil || !p.Enabled {
		return nil
	}
	refs := make([]wordRuleRef, 0, len(p.Rules))
	for _, rule := range p.Rules {
		if rule.Enabled {
			refs = append(refs, wordRuleRef{WordRule: rule, Layer: layer})
		}
	}
	return refs
}

// buildWordContexts turns the policies that govern a request into the
// contexts it is checked against. Any of the three may be nil (never saved).
//
// The platform policy forms a context of its own. The tenant policy and the
// key policy form one: the key's own rules are added to the tenant's (unless
// the key opts out of the tenant's rules with inherit_tenant=false) and the
// key's settings override the tenant's. A disabled policy contributes nothing
// to the context it would have joined; the platform policy is never affected by
// the other two.
func buildWordContexts(platform, tenant, key *WordPolicy) []wordContext {
	var contexts []wordContext
	if platform != nil && platform.Enabled {
		if matcher := compiledWordMatcher(ruleRefs(platform, WordScopePlatform), mergeWordSettings(platform).fold); matcher != nil {
			contexts = append(contexts, newWordContext(WordScopePlatform, mergeWordSettings(platform), matcher))
		}
	}
	tenantActive := tenant != nil && tenant.Enabled
	keyActive := key != nil && key.Enabled
	var refs []wordRuleRef
	var layers []*WordPolicy
	layer := ""
	if tenantActive && (!keyActive || key.InheritTenant) {
		refs = append(refs, ruleRefs(tenant, WordScopeTenant)...)
		layers = append(layers, tenant)
		layer = WordScopeTenant
	} else if tenantActive {
		// The key opted out of the tenant's rules, but its settings still apply on
		// top of the tenant's: a key that "replaces" the tenant list keeps the
		// tenant's action unless it names its own.
		layers = append(layers, tenant)
	}
	if keyActive {
		refs = append(refs, ruleRefs(key, WordScopeKey)...)
		layers = append(layers, key)
		layer = WordScopeKey
	}
	if len(refs) > 0 {
		settings := mergeWordSettings(layers...)
		if matcher := compiledWordMatcher(refs, settings.fold); matcher != nil {
			contexts = append(contexts, newWordContext(layer, settings, matcher))
		}
	}
	return contexts
}

func newWordContext(layer string, settings wordSettings, matcher *wordMatcher) wordContext {
	return wordContext{
		layer:        layer,
		action:       settings.action,
		replacement:  settings.replacement,
		scanScope:    settings.scanScope,
		blockMessage: settings.blockMessage,
		fold:         settings.fold,
		reply:        settings.reply,
		matcher:      matcher,
	}
}

// maxCachedWordContexts bounds wordContextCache. The entries alive at one time
// are about as many as there are tenants and API keys with a policy; when the
// bound is reached the cache starts over and the next requests rebuild what they
// need. It matches the bound on compiled matchers (maxCachedWordMatchers),
// which the entries keep alive.
const maxCachedWordContexts = maxCachedWordMatchers

// wordPolicyStamp names one saved state of a stored policy. Every save raises
// the version and sets the time of the last save, and a policy that is deleted
// and created again starts over at version 1 with a new time, so the two
// together tell states apart (the policy cache in the store relies on the same
// pair). The zero stamp stands for a policy that does not exist.
type wordPolicyStamp struct {
	tenantID, scopeType, scopeID string
	version                      int64
	savedAt                      int64 // Unix nanoseconds
}

// stampWordPolicy returns the stamp of a policy read from the store. A policy
// without a version or a save time (a draft, a policy built by hand) cannot be
// told apart from a changed copy of itself, so it has no stamp and is never
// cached.
func stampWordPolicy(p *WordPolicy) (wordPolicyStamp, bool) {
	if p == nil {
		return wordPolicyStamp{}, true
	}
	if p.Version <= 0 || p.UpdatedAt.IsZero() {
		return wordPolicyStamp{}, false
	}
	return wordPolicyStamp{p.TenantID, p.ScopeType, p.ScopeID, p.Version, p.UpdatedAt.UnixNano()}, true
}

type wordContextKey struct{ platform, tenant, key wordPolicyStamp }

// wordContextCache remembers the contexts built from stored policies. Building
// them copies and fingerprints every rule of every layer (with a full-size list
// of 10,000 rules that is over a millisecond and a couple of megabytes of
// garbage per request) only to arrive at the matcher the previous request
// already used. Stored policies change only when saved, and a save changes the
// stamp, so a request that finds its stamps here can use the contexts as they
// are. The zero value is ready to use.
type wordContextCache struct {
	mu      sync.Mutex
	entries map[wordContextKey][]wordContext
}

// contextsFor is buildWordContexts with the result remembered while the
// policies keep the stamps it was built for. The result is shared between
// requests and must not be modified.
func (c *wordContextCache) contextsFor(platform, tenant, key *WordPolicy) []wordContext {
	if platform == nil && tenant == nil && key == nil {
		return nil
	}
	var k wordContextKey
	var okPlatform, okTenant, okKey bool
	k.platform, okPlatform = stampWordPolicy(platform)
	k.tenant, okTenant = stampWordPolicy(tenant)
	k.key, okKey = stampWordPolicy(key)
	if !okPlatform || !okTenant || !okKey {
		return buildWordContexts(platform, tenant, key)
	}
	c.mu.Lock()
	cached, found := c.entries[k]
	c.mu.Unlock()
	if found {
		return cached
	}
	built := buildWordContexts(platform, tenant, key)
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= maxCachedWordContexts {
		c.entries = make(map[wordContextKey][]wordContext)
	}
	c.entries[k] = built
	c.mu.Unlock()
	return built
}
