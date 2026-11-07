package contentmoderation

import "strings"

// wordContext is what applies together to one request: a set of rules and the
// settings that say how to scan for them and what a hit does.
//
// A request has at most three. The platform's own context always comes first
// and nothing below it can touch it, because letting a tenant or a key change
// the platform's action or scope would be the same as letting them switch it
// off. The tenant's context follows, with the API key's adjustments folded in.
// Last comes the context of the portal user who owns the key, when the tenant
// lets the user keep a policy: it is the user's own and stands apart for the
// same reason, so that nothing the user sets weakens the two before it.
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
	if context, ok := buildPlatformContext(platform); ok {
		contexts = append(contexts, context)
	}
	if context, ok := buildTenantContext(tenant, key); ok {
		contexts = append(contexts, context)
	}
	return contexts
}

// buildPolicyContexts is buildWordContexts for all the policies of a request,
// the portal user's included.
func buildPolicyContexts(policies WordPolicies) []wordContext {
	contexts := buildWordContexts(policies.Platform, policies.Tenant, policies.Key)
	if context, ok := buildEndUserContext(policies.EndUser); ok {
		contexts = append(contexts, context)
	}
	return contexts
}

// buildPlatformContext is the context of the platform policy, if it has one.
func buildPlatformContext(platform *WordPolicy) (wordContext, bool) {
	return buildStandaloneContext(platform, WordScopePlatform)
}

// buildEndUserContext is the context of a portal user's policy, if it has one.
// Whether the tenant allows the user one is settled before: a policy that is
// not allowed is not passed in (see Store.readWordPolicies).
func buildEndUserContext(endUser *WordPolicy) (wordContext, bool) {
	return buildStandaloneContext(endUser, WordScopeEndUser)
}

// buildStandaloneContext is the context of a policy that is applied on its own,
// with its own settings over the defaults and nothing else.
func buildStandaloneContext(policy *WordPolicy, layer string) (wordContext, bool) {
	if policy == nil || !policy.Enabled {
		return wordContext{}, false
	}
	settings := mergeWordSettings(policy)
	matcher := newWordMatcher(ruleRefs(policy, layer), settings.fold)
	if matcher == nil {
		return wordContext{}, false
	}
	return newWordContext(layer, settings, matcher), true
}

// buildTenantContext is the context of the tenant policy with the key's
// adjustments folded in, if there is one. Only an enabled policy takes part:
// what it decides is the same for any state of a disabled one.
func buildTenantContext(tenant, key *WordPolicy) (wordContext, bool) {
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
	if len(refs) == 0 {
		return wordContext{}, false
	}
	settings := mergeWordSettings(layers...)
	matcher := newWordMatcher(refs, settings.fold)
	if matcher == nil {
		return wordContext{}, false
	}
	return newWordContext(layer, settings, matcher), true
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
