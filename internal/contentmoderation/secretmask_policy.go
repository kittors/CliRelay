package contentmoderation

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
)

// MaskPolicy is what privacy masking does to the requests it applies to: a
// tenant's default, or a portal user's own (see effectiveMaskOptions for how
// the two combine). It is the panel's settings page, field for field.
type MaskPolicy struct {
	// Secrets masks API keys, private keys, tokens and passwords.
	Secrets bool `json:"secrets"`
	// Personal masks emails, phone numbers, ID and bank card numbers, and the
	// categories of Kinds that are on.
	Personal bool `json:"personal"`
	// Kinds turns each of privacymask.Categories on or off by its id, where
	// the user chose; one not in it is as the category's default says.
	Kinds        map[string]bool `json:"kinds"`
	Words        []string        `json:"words"`
	WordsAnyCase bool            `json:"words_any_case"`
	WordsWhole   bool            `json:"words_whole"`
	Rules        []MaskRule      `json:"rules"`
	// Allow are values that always go as written.
	Allow []string `json:"allow"`
	// Actions is what is done with a value of a kind other than masking it:
	// "warn" sends it as written and records it, "block" holds the request
	// back. Only the kinds not masked are in it.
	Actions map[string]string `json:"actions"`
}

// MaskRule is a rule of the tenant's or the user's own: a prefix or an RE2
// pattern.
type MaskRule = privacymask.Rule

// How much a policy may hold. Every request of the tenant, or the user, runs
// through it, and the words are matched in every string of each.
const (
	maxMaskWords    = 500
	maxMaskWordLen  = 200
	maxMaskAllow    = 200
	maxMaskAllowLen = 512
	maxMaskActions  = 64
)

// Enforceable categories: a tenant may turn them on for all of its portal
// users, who then cannot switch them off.
const (
	EnforceSecrets  = "secrets"
	EnforcePersonal = "personal"
)

// ErrInvalidMaskPolicy wraps what is wrong with a policy a client sent.
var ErrInvalidMaskPolicy = errors.New("invalid privacy masking settings")

func invalidPolicy(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMaskPolicy, fmt.Sprintf(format, args...))
}

// Normalize tidies p (spaces trimmed, the empty and the repeated dropped, a
// rule's kind as a placeholder writes it) and says what is wrong with a part
// that cannot be used. Nil lists come back empty, as the panel reads them.
func (p *MaskPolicy) Normalize() error {
	words, err := tidyList(p.Words, maxMaskWords, maxMaskWordLen, "masking words")
	if err != nil {
		return err
	}
	p.Words = words
	allow, err := tidyList(p.Allow, maxMaskAllow, maxMaskAllowLen, "values never masked")
	if err != nil {
		return err
	}
	p.Allow = allow
	rules, err := privacymask.CheckRules(p.Rules)
	if err != nil {
		return invalidPolicy("%v", err)
	}
	if rules == nil {
		rules = []MaskRule{}
	}
	p.Rules = rules
	kinds := make(map[string]bool, len(p.Kinds))
	for id, on := range p.Kinds {
		if !slices.ContainsFunc(privacymask.Categories, func(c privacymask.Category) bool { return c.ID == id }) {
			return invalidPolicy("there is no category of personal data called %q", id)
		}
		kinds[id] = on
	}
	p.Kinds = kinds
	if len(p.Actions) > maxMaskActions {
		return invalidPolicy("at most %d kinds may have an action of their own", maxMaskActions)
	}
	actions := make(map[string]string, len(p.Actions))
	for kind, action := range p.Actions {
		if !privacymask.KindName(kind) {
			return invalidPolicy("%q is not a kind a value can have", kind)
		}
		switch action {
		case privacymask.Mask, "":
			// masking is what is done without an entry
		case privacymask.Warn, privacymask.Block:
			actions[kind] = action
		default:
			return invalidPolicy("the action of %s is mask, warn or block, not %q", kind, action)
		}
	}
	p.Actions = actions
	return nil
}

// tidyList trims each entry, drops the empty and the repeated, and checks the
// count and the length of each.
func tidyList(in []string, maxCount, maxLen int, what string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		if utf8.RuneCountInString(v) > maxLen {
			return nil, invalidPolicy("each of the %s is at most %d characters long", what, maxLen)
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) > maxCount {
		return nil, invalidPolicy("at most %d %s, not %d", maxCount, what, len(out))
	}
	return out, nil
}

// NormalizeEnforced checks the categories a tenant enforces and returns them
// in a fixed order.
func NormalizeEnforced(in []string) ([]string, error) {
	out := []string{}
	for _, id := range []string{EnforceSecrets, EnforcePersonal} {
		if slices.Contains(in, id) {
			out = append(out, id)
		}
	}
	for _, id := range in {
		if id != EnforceSecrets && id != EnforcePersonal {
			return nil, invalidPolicy("only %q and %q can be enforced, not %q", EnforceSecrets, EnforcePersonal, id)
		}
	}
	return out, nil
}

// options is p as masking runs it, for the requests p alone applies to.
func (p MaskPolicy) options() privacymask.Options {
	rules := p.Rules
	return privacymask.Options{
		Secrets: p.Secrets, Personal: p.Personal, Kinds: p.Kinds,
		Words: p.Words, WordsAnyCase: p.WordsAnyCase, WordsWhole: p.WordsWhole,
		Rules: rules, Allow: p.Allow, Actions: p.Actions,
	}
}

// any says p masks anything at all.
func (p MaskPolicy) any() bool { return p.options().Any() }

// effectiveMaskOptions is what masking runs for a request: the tenant's
// default policy, or the portal user's own one when there is one the tenant
// allows (user), combined so that what the tenant decided for everyone holds.
// portal says the request is a portal user's: what the tenant enforces holds
// for them even without a policy of their own, while a request no portal user
// sent gets the tenant's default as it is.
//
//   - an enforced category is on, and its values are masked or held back,
//     never sent as written: the user's warn for one of its kinds and the
//     user's "never mask" list do not apply to it;
//   - the tenant's words, rules and "never mask" list always apply, the user's
//     own come on top; a tenant's word cannot be let go by the user;
//   - with personal data enforced, a category the tenant has on stays on.
//
// The rest is the user's.
func effectiveMaskOptions(tenant SecretMaskingSettings, user *MaskPolicy, portal bool) privacymask.Options {
	base := tenant.policy()
	if user == nil {
		if !portal || len(tenant.Enforced) == 0 {
			return base.options()
		}
		// the tenant's default as the user's own: its words, rules and "never
		// mask" list come in as the tenant's below
		own := base
		own.Words, own.Rules, own.Allow = nil, nil, nil
		user = &own
	}
	enforced := tenant.Enforced
	secretsLocked := slices.Contains(enforced, EnforceSecrets)
	personalLocked := slices.Contains(enforced, EnforcePersonal)
	o := privacymask.Options{
		Secrets:  user.Secrets || secretsLocked,
		Personal: user.Personal || personalLocked,
		Words:    union(base.Words, user.Words),
		// matching more never lets a word through: any case if either asks,
		// whole words only if both do
		WordsAnyCase: user.WordsAnyCase || len(base.Words) > 0 && base.WordsAnyCase,
		WordsWhole:   user.WordsWhole && (len(base.Words) == 0 || base.WordsWhole),
		Rules:        append(slices.Clone(base.Rules), user.Rules...),
		Allow:        base.Allow,
		UserAllow:    user.Allow,
		Locked:       map[string]bool{"word": len(base.Words) > 0, "secret": secretsLocked, "personal": personalLocked},
	}
	o.Kinds = make(map[string]bool, len(user.Kinds)+len(base.Kinds))
	for id, on := range user.Kinds {
		o.Kinds[id] = on
	}
	if personalLocked {
		for _, c := range privacymask.Categories {
			if on, ok := base.Kinds[c.ID]; ok && on || !ok && c.On {
				o.Kinds[c.ID] = true
			}
		}
	}
	o.Actions = make(map[string]string, len(user.Actions)+len(base.Actions))
	for kind, action := range user.Actions {
		o.Actions[kind] = action
	}
	for kind, action := range o.Actions {
		if action == privacymask.Warn && o.Locked[privacymask.Class(kind)] {
			// sent as written is what enforcing rules out: the tenant's own
			// choice for the kind applies, unless it is warn too
			o.Actions[kind] = privacymask.Mask
			if base.Actions[kind] == privacymask.Block {
				o.Actions[kind] = privacymask.Block
			}
		}
	}
	for kind, action := range base.Actions {
		if action == privacymask.Block && privacymask.Class(kind) == "word" {
			o.Actions[kind] = privacymask.Block // the tenant's words: its choice
		}
	}
	return o
}

// union is a followed by what b adds to it.
func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// MaskPreview is what masking makes of a text a client tries out.
type MaskPreview struct {
	Text    string            `json:"text"`
	Values  []MaskRecordValue `json:"values"`
	Blocked bool              `json:"blocked"`
}

// MaxMaskPreviewBytes bounds a text tried out.
const MaxMaskPreviewBytes = 64 << 10

// PreviewMask masks text the way a request under tenant's settings, and the
// portal user's policy user (nil: the tenant's default), would be masked, but
// with a key of its own, made for the preview: the placeholders it shows are
// not the tenant's, so a preview cannot tell which value a record's
// placeholder stands for. Nothing of it is kept.
func PreviewMask(tenant SecretMaskingSettings, user *MaskPolicy, text string) MaskPreview {
	key := make([]byte, secretMaskingKeyBytes)
	_, _ = rand.Read(key)
	masker := privacymask.NewMasker(key, effectiveMaskOptions(tenant, user, user != nil), nil, "")
	out := MaskPreview{Text: text, Values: []MaskRecordValue{}}
	edits := masker.Plan([]string{text}, nil)[0]
	out.Text = privacymask.Apply(text, edits)
	for _, v := range masker.Values() {
		out.Values = append(out.Values, MaskRecordValue{Kind: v.Kind, By: v.By, Action: v.Action, Head: v.Head, Len: v.Len, N: v.N, Placeholder: v.Placeholder})
	}
	out.Blocked = len(masker.Blocked()) > 0
	return out
}
