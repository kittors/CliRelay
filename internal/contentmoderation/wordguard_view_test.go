package contentmoderation

import "testing"

func TestWordDefaults(t *testing.T) {
	got := WordDefaults()
	want := WordSettings{Action: WordActionBlock, Replacement: DefaultWordReplacement, ScanScope: WordScanAllUser, BlockMessage: DefaultBlockMessage}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
}

func TestResolveWordSettings(t *testing.T) {
	tenant := &WordPolicy{Enabled: true, Action: WordActionReplace, Replacement: "[x]", ReplyFilter: boolPtr(true), ScanScope: WordScanWithSystem}
	key := &WordPolicy{Enabled: true, Action: WordActionLog}

	got := ResolveWordSettings(tenant, key)
	if got.Action != WordActionLog || got.Replacement != "[x]" || !got.ReplyFilter || got.ScanScope != WordScanWithSystem {
		t.Fatalf("a key policy takes what it sets and the tenant's for the rest: %+v", got)
	}
	if got := ResolveWordSettings(nil, tenant); got.Action != WordActionReplace || got.BlockMessage != DefaultBlockMessage {
		t.Fatalf("a policy alone is filled in from the defaults: %+v", got)
	}

	disabled := *tenant
	disabled.Enabled = false
	if got := ResolveWordSettings(&disabled, key); got.Replacement != DefaultWordReplacement || got.ReplyFilter || got.ScanScope != WordScanAllUser {
		t.Fatalf("a disabled tenant policy contributes nothing to a key: %+v", got)
	}
	off := *key
	off.Enabled = false
	if got := ResolveWordSettings(nil, &off); got.Action != WordActionLog {
		t.Fatalf("a disabled policy's own settings still show what they mean: %+v", got)
	}
}

// Whatever the request path does with a key that keeps its own rules, the
// settings shown for it must be the ones a request is checked with.
func TestResolveWordSettingsMatchesWhatARequestUses(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action, p.Replacement, p.ScanScope, p.ReplyFilter = WordActionReplace, "[t]", WordScanWithTools, boolPtr(true)
	}, kw("tenantword"))
	for _, inherit := range []bool{true, false} {
		key := policyOf(t, WordScopeKey, "k1", func(p *WordPolicy) { p.InheritTenant = inherit; p.Action = WordActionLog }, kw("keyword"))
		contexts := buildWordContexts(nil, tenant, key)
		if len(contexts) != 1 {
			t.Fatalf("inherit=%v: %d contexts", inherit, len(contexts))
		}
		used, shown := contexts[0], ResolveWordSettings(tenant, key)
		if used.action != shown.Action || used.replacement != shown.Replacement || used.scanScope != shown.ScanScope || used.reply != shown.ReplyFilter {
			t.Fatalf("inherit=%v: a request uses %+v, the view says %+v", inherit, used, shown)
		}
	}
}
