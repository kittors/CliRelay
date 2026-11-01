package contentmoderation

import (
	"regexp"
	"sort"
	"strings"
)

// Outbound secret masking, first stage: secrets only.
//
// A tenant may switch it on to keep the credentials that travel in its
// requests away from the upstream: a key pasted into a prompt, a .env file a
// coding agent read, a token in a tool result. Every secret found in the text
// the model reads is replaced with a placeholder before the request leaves, and
// the placeholder is turned back into the secret in the reply, so the client
// sees its own text again (secretmask_reply.go). It protects against the
// upstream and any relay in front of it, not against this server.
//
// Only what is clearly a secret is matched: a provider key by its prefix, a
// JWT, a bearer token, the password of a connection string, and the value of an
// assignment whose name says it is a secret. A false positive costs the user a
// model that cannot see a value it needs, so the rules would rather miss a
// secret than mask ordinary text; the tests hold the cases other masking
// gateways got wrong (sort_key, max_tokens, sk-hynix in a URL,
// sequential_cutoff).
//
// A masked value is made of characters JSON never escapes and that cannot end
// a string or a token (secretValueChar). Its placeholder can then be swapped
// back anywhere in a reply, in a text, in the JSON of a tool call's arguments or
// in a string inside that JSON, without escaping anything.

// secretKind names what a placeholder stands for; it is part of the placeholder.
type secretKind string

const (
	secretAPIKey   secretKind = "API_KEY"
	secretJWT      secretKind = "JWT"
	secretToken    secretKind = "TOKEN"
	secretPassword secretKind = "PASSWORD"
	secretValue    secretKind = "SECRET"
)

// secretSpan is a secret found in a text: text[start:end].
type secretSpan struct {
	start, end int
	kind       secretKind
}

// secretRule finds one kind of secret.
type secretRule struct {
	kind secretKind
	// hints are substrings of which a text has to contain one for the rule to be
	// worth running.
	hints []string
	// fold says the hints are lower case and are looked for in the text folded
	// to lower case.
	fold bool
	re   *regexp.Regexp
	// group is the submatch holding the secret; 0 is the whole match.
	group int
	// bounded says the secret must not continue a word on either side: a key
	// prefix inside a longer identifier is not a key.
	bounded bool
	// assignment says the value is a name's value, which code often fills with
	// something that is not a secret at all (see plausibleAssignedSecret).
	assignment bool
	// kindOf, when set, decides the kind from the match.
	kindOf func(match string) secretKind
}

var secretRules = []secretRule{
	{
		kind:    secretAPIKey,
		hints:   []string{"sk-", "sk_", "rk_", "AIza", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xox", "AKIA", "ASIA", "hf_", "xai-", "gsk_", "npm_"},
		bounded: true,
		re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{32,}` +
			`|sk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{32,}` +
			`|sk-[A-Za-z0-9]{32,}` +
			`|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{20,}` +
			`|AIza[0-9A-Za-z_-]{35}` +
			`|gh[pousr]_[A-Za-z0-9]{36,}` +
			`|github_pat_[A-Za-z0-9_]{50,}` +
			`|glpat-[A-Za-z0-9_-]{20,}` +
			`|xox[abposr]-[A-Za-z0-9-]{10,}` +
			`|(?:AKIA|ASIA)[0-9A-Z]{16}` +
			`|hf_[A-Za-z0-9]{30,}` +
			`|xai-[A-Za-z0-9]{40,}` +
			`|gsk_[A-Za-z0-9]{40,}` +
			`|npm_[A-Za-z0-9]{36,}`),
	},
	{
		kind:    secretJWT,
		hints:   []string{"eyJ"},
		bounded: true,
		re:      regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}`),
	},
	{
		kind:  secretToken,
		hints: []string{"bearer"},
		fold:  true,
		re:    regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{20,})`),
		group: 1,
	},
	{
		kind:  secretPassword,
		hints: []string{"://"},
		// scheme://user:password@host. The password stops at the characters that
		// end the userinfo; one that holds anything else is left alone.
		re:    regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s:/@"'<>]+:([^\s:/@"'<>]{4,})@`),
		group: 1,
	},
	{
		kind:       secretValue,
		hints:      []string{"pass", "pwd", "secret", "key", "token"},
		fold:       true,
		assignment: true,
		// name = value, name: value, "name": "value", with the name's own
		// boundary on the left: MY_API_KEY= counts, max_tokens: does not (no
		// separator follows the name), and neither does a name inside another word.
		re:    regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(password|passwd|passphrase|pwd|secret|client[_-]?secret|secret[_-]?key|api[_-]?key|apikey|access[_-]?key|access[_-]?token|auth[_-]?token|refresh[_-]?token|private[_-]?key|token)["']?\s*[:=]\s*["']?([A-Za-z0-9_.~+/=!@#$%^*:-]{8,})`),
		group: 2,
		kindOf: func(match string) secretKind {
			lower := strings.ToLower(match)
			if strings.Contains(lower, "pass") || strings.Contains(lower, "pwd") {
				return secretPassword
			}
			return secretValue
		},
	},
	{
		kind:       secretPassword,
		hints:      []string{"密码", "口令", "密钥", "秘钥", "私钥", "令牌"},
		assignment: true,
		re:         regexp.MustCompile(`(密码|口令|密钥|秘钥|私钥|令牌)\s*(?:[:：=]|是|为)\s*([A-Za-z0-9_.~+/=!@#$%^*:-]{6,})`),
		group:      2,
		kindOf: func(match string) secretKind {
			if strings.HasPrefix(match, "密码") || strings.HasPrefix(match, "口令") {
				return secretPassword
			}
			return secretValue
		},
	},
}

// findSecrets returns the secrets in text, in order and not overlapping.
func findSecrets(text string) []secretSpan {
	var spans []secretSpan
	var lower string
	for _, rule := range secretRules {
		if !rule.mayMatch(text, &lower) {
			continue
		}
		for _, loc := range rule.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := loc[2*rule.group], loc[2*rule.group+1]
			if start < 0 || end <= start {
				continue
			}
			value := text[start:end]
			if !isSecretValue(value) {
				continue
			}
			if rule.bounded && (start > 0 && isSecretWordByte(text[start-1]) || end < len(text) && isSecretWordByte(text[end])) {
				continue
			}
			if rule.assignment && !plausibleAssignedSecret(value, text[end:]) {
				continue
			}
			kind := rule.kind
			if rule.kindOf != nil {
				// Group 1 of those rules is the name.
				kind = rule.kindOf(text[loc[2]:loc[3]])
			}
			spans = append(spans, secretSpan{start: start, end: end, kind: kind})
		}
	}
	if len(spans) < 2 {
		return spans
	}
	// The first rule to find a secret keeps it: a key the prefix rule knows is
	// an API key even when it is also some name's value.
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	kept := spans[:1]
	for _, span := range spans[1:] {
		if span.start < kept[len(kept)-1].end {
			continue
		}
		kept = append(kept, span)
	}
	return kept
}

func (r secretRule) mayMatch(text string, lower *string) bool {
	haystack := text
	if r.fold {
		if *lower == "" {
			*lower = strings.ToLower(text)
		}
		haystack = *lower
	}
	for _, hint := range r.hints {
		if strings.Contains(haystack, hint) {
			return true
		}
	}
	return false
}

// isSecretValue reports whether value may be masked: every byte one of
// secretValueChar's.
func isSecretValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if !secretValueChar(value[i]) {
			return false
		}
	}
	return value != ""
}

// secretValueChar is a byte a masked value may hold. JSON escapes none of them,
// and none ends a string, so the value can stand in for its placeholder
// anywhere in a reply as it is.
func secretValueChar(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	return strings.IndexByte("_.~+/=!@#$%^&*?:-", c) >= 0
}

func isSecretWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// plausibleAssignedSecret reports whether the value of a name that says it is
// a secret is likely to be one, after is the text that follows it. Code and
// configuration fill such names with all sorts of things that are not: a
// variable (process.env.OPENAI_API_KEY, os.environ), a call (getToken()), a
// template ($API_KEY, {{key}}, <your-key>), a dummy (********, xxxxxxxx,
// your_api_key), or a plain word (required). A secret has a digit or a symbol
// in it, or both cases.
func plausibleAssignedSecret(value, after string) bool {
	if after != "" && (after[0] == '(' || after[0] == '[') {
		return false
	}
	switch value[0] {
	case '$', '{', '<', '%', '*':
		return false
	}
	lower := strings.ToLower(value)
	for _, dummy := range []string{"your", "example", "xxxx", "****", "placeholder", "changeme", "redacted", "<", "${"} {
		if strings.Contains(lower, dummy) {
			return false
		}
	}
	if isDottedIdentifier(value) {
		return false
	}
	var digit, symbol, upper, lowerCase bool
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'z':
			lowerCase = true
		case c >= 'A' && c <= 'Z':
			upper = true
		default:
			symbol = true
		}
	}
	return digit || symbol || upper && lowerCase
}

// isDottedIdentifier reports whether value reads like a reference in code:
// identifiers joined by dots.
func isDottedIdentifier(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part[0] >= '0' && part[0] <= '9' {
			return false
		}
		for i := 0; i < len(part); i++ {
			if c := part[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return false
			}
		}
	}
	return true
}
