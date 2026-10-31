// Package loopguard holds the loop protection settings of tenants and API keys
// and resolves them into what the gateway does with one request.
//
// Loop protection is two checks, ending a streamed reply that has become the
// same few lines over and over and answering a tool-call loop without sending
// it upstream; their detection is sdk/loopdetect and their application the
// stream handlers in sdk/api/handlers. This package decides only whether and
// how strictly each check runs for a request. Both are off until a tenant, or
// one of its API keys, turns them on: a verdict on the model's output cuts a
// reply, so no tenant gets it without asking.
package loopguard

import (
	"maps"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
)

// SettingKey is the runtime_settings row of a tenant holding its Settings.
const SettingKey = "loop-guard"

func init() {
	// The setting's receiver is this package, not a config field, so a write
	// is announced under its own domain and the other nodes reload just this
	// tenant's settings.
	configsync.RouteRuntimeSettingKey(SettingKey, configsync.DomainLoopGuard)
}

// ReplyRule is the setting of the repeated-reply check at one level.
type ReplyRule struct {
	// Enabled turns the check on or off. At the tenant level nil is off. At an
	// API key nil follows the tenant, and a value overrides it either way: a
	// key can be exempted from a tenant that has the check on.
	Enabled *bool `json:"enabled,omitempty"`
	// Window and MaxDistinct are the thresholds (see loopdetect.ReplyLimits).
	// Zero follows the level above, the tenant for a key, and then the default.
	Window      int `json:"window,omitempty"`
	MaxDistinct int `json:"max_distinct,omitempty"`
}

// ToolRule is the setting of the tool-call loop check at one level.
type ToolRule struct {
	// Enabled is read like ReplyRule.Enabled.
	Enabled *bool `json:"enabled,omitempty"`
	// Repeats is the threshold (see loopdetect.ToolLimits); zero follows the
	// level above and then the default.
	Repeats int `json:"repeats,omitempty"`
}

// Scope is the rules of one level, the tenant's or one API key's.
type Scope struct {
	Reply ReplyRule `json:"reply"`
	Tool  ToolRule  `json:"tool"`
}

// IsZero says whether the scope sets nothing, which for an API key means it
// follows its tenant in everything.
func (s Scope) IsZero() bool {
	return s.Reply == ReplyRule{} && s.Tool == ToolRule{}
}

// Validate refuses a threshold outside its bounds, with the reason.
func (s Scope) Validate() error {
	if err := (loopdetect.ReplyLimits{Window: s.Reply.Window, Distinct: s.Reply.MaxDistinct}).Validate(); err != nil {
		return err
	}
	return loopdetect.ToolLimits{Repeats: s.Tool.Repeats}.Validate()
}

// Settings is the stored form, one row per tenant. The embedded Scope is the
// tenant's own; Keys holds the overrides of its API keys by key id.
type Settings struct {
	Scope
	Keys      map[string]Scope `json:"keys,omitempty"`
	UpdatedAt time.Time        `json:"updated_at,omitzero"`
}

// Clone returns a copy that shares no map with s.
func (s Settings) Clone() Settings {
	s.Keys = maps.Clone(s.Keys)
	return s
}

// Sanitized returns s without what carries no meaning: an override that sets
// nothing, and a threshold below zero. Stored rows are written by this code
// only, but a hand-edited one must not turn a check on at a nonsensical
// threshold.
func (s Settings) Sanitized() Settings {
	s = s.Clone()
	s.Scope = s.Scope.sanitized()
	for id, scope := range s.Keys {
		if scope = scope.sanitized(); scope.IsZero() || id == "" {
			delete(s.Keys, id)
		} else {
			s.Keys[id] = scope
		}
	}
	if len(s.Keys) == 0 {
		s.Keys = nil
	}
	return s
}

func (s Scope) sanitized() Scope {
	s.Reply.Window = max(s.Reply.Window, 0)
	s.Reply.MaxDistinct = max(s.Reply.MaxDistinct, 0)
	s.Tool.Repeats = max(s.Tool.Repeats, 0)
	return s
}

// Source says which level switched a check on for a request.
type Source string

const (
	SourceTenant Source = "tenant"
	SourceAPIKey Source = "api_key"
)

// Effective is what loop protection does for one request: a check is nil when
// it is off.
type Effective struct {
	Reply       *loopdetect.ReplyLimits
	ReplySource Source
	Tool        *loopdetect.ToolLimits
	ToolSource  Source
}

// Any says whether either check is on.
func (e Effective) Any() bool { return e.Reply != nil || e.Tool != nil }

// Effective resolves the settings for the API key with the given id: its own
// override first, then the tenant's, then the defaults. An empty id (a request
// that carries no key of this tenant) follows the tenant.
func (s Settings) Effective(keyID string) Effective {
	var override Scope
	if keyID != "" {
		override = s.Keys[keyID]
	}
	var e Effective

	if on, source := resolveEnabled(s.Reply.Enabled, override.Reply.Enabled); on {
		limits := loopdetect.ReplyLimits{
			Window:   firstSet(override.Reply.Window, s.Reply.Window),
			Distinct: firstSet(override.Reply.MaxDistinct, s.Reply.MaxDistinct),
		}.Normalized()
		e.Reply, e.ReplySource = &limits, source
	}
	if on, source := resolveEnabled(s.Tool.Enabled, override.Tool.Enabled); on {
		limits := loopdetect.ToolLimits{Repeats: firstSet(override.Tool.Repeats, s.Tool.Repeats)}.Normalized()
		e.Tool, e.ToolSource = &limits, source
	}
	return e
}

// resolveEnabled applies an API key's switch over its tenant's.
func resolveEnabled(tenant, key *bool) (bool, Source) {
	if key != nil {
		return *key, SourceAPIKey
	}
	return tenant != nil && *tenant, SourceTenant
}

func firstSet(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

// Bool returns a pointer to b, for the switches of a rule.
func Bool(b bool) *bool { return &b }
