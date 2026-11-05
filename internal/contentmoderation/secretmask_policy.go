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

// MaskPolicy is what privacy masking does to a tenant's requests. It is the
// tenant's call alone: portal users only see it (privacy_masking_portal.go).
// It is the panel's settings page, field for field.
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

// MaskRule is a rule of the tenant's own: a prefix or an RE2 pattern.
type MaskRule = privacymask.Rule

// How much a policy may hold. Every request of the tenant runs through it, and the words are matched in every string of each.
const (
	maxMaskWords    = 500
	maxMaskWordLen  = 200
	maxMaskAllow    = 200
	maxMaskAllowLen = 512
	maxMaskActions  = 64
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

// options is p as masking runs it.
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

// MaskPreview is what masking makes of a text a client tries out.
type MaskPreview struct {
	Text    string            `json:"text"`
	Values  []MaskRecordValue `json:"values"`
	Blocked bool              `json:"blocked"`
}

// MaxMaskPreviewBytes bounds a text tried out.
const MaxMaskPreviewBytes = 64 << 10

// PreviewMask masks text the way a request under policy would be masked, but
// with a key of its own, made for the preview: the placeholders it shows are
// not the tenant's, so a preview cannot tell which value a record's
// placeholder stands for. Nothing of it is kept.
func PreviewMask(policy MaskPolicy, text string) MaskPreview {
	key := make([]byte, secretMaskingKeyBytes)
	_, _ = rand.Read(key)
	masker := privacymask.NewMasker(key, policy.options(), nil, "")
	out := MaskPreview{Text: text, Values: []MaskRecordValue{}}
	edits := masker.Plan([]string{text}, nil)[0]
	out.Text = privacymask.Apply(text, edits)
	for _, v := range masker.Values() {
		out.Values = append(out.Values, MaskRecordValue{Kind: v.Kind, By: v.By, Action: v.Action, Head: v.Head, Len: v.Len, N: v.N, Placeholder: v.Placeholder})
	}
	out.Blocked = len(masker.Blocked()) > 0
	return out
}
