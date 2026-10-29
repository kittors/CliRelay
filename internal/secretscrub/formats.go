package secretscrub

import "strings"

// maxTokenLen bounds a credential's length; a longer run of the same
// characters is data (an inline file, a hash list), not a credential.
const maxTokenLen = 16 << 10

// maxPEMLen bounds the body of a PEM private key: an 8192-bit RSA key in PEM
// is under 7 KB, with room left for escaped line breaks.
const maxPEMLen = 32 << 10

// prefixRule recognises a credential format that starts with a fixed prefix.
type prefixRule struct {
	prefix string
	kind   string
	body   func(byte) bool // the characters after the prefix
	min    int             // fewest characters after the prefix
	max    int             // most characters after the prefix; 0 for no limit
	check  func(body string) bool
}

func isAlnumDash(c byte) bool    { return isAlnum(c) || c == '-' }
func isAlnumUnder(c byte) bool   { return isAlnum(c) || c == '_' }
func isKeyCharDot(c byte) bool   { return isKeyChar(c) || c == '.' }
func isAlnumSlash(c byte) bool   { return isAlnum(c) || c == '/' }
func isUpperDigit(c byte) bool   { return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' }
func isLowerHexOnly(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

// prefixRules lists the formats with a fixed prefix. Where one prefix starts
// another (sk-ant- and sk-), the longer comes first: the first rule that
// matches at a position wins.
var prefixRules = []prefixRule{
	{prefix: "sk-ant-", kind: "anthropic_key", body: isKeyChar, min: 32},
	{prefix: "sk-proj-", kind: "openai_key", body: isKeyChar, min: 20},
	{prefix: "sk-svcacct-", kind: "openai_key", body: isKeyChar, min: 20},
	{prefix: "sk-admin-", kind: "openai_key", body: isKeyChar, min: 20},
	{prefix: "sk-or-v1-", kind: "openrouter_key", body: isLowerHexOnly, min: 32},
	{prefix: "sk_live_", kind: "stripe_key", body: isAlnum, min: 20},
	{prefix: "sk_test_", kind: "stripe_key", body: isAlnum, min: 20},
	// OpenAI's older keys, DeepSeek, Moonshot, Qwen, SiliconFlow and this
	// relay's own client keys: sk- and 32 or more letters and digits. The body
	// takes no dash, which keeps out slugs such as /news/sk-hynix-hbm4-….
	{prefix: "sk-", kind: "api_key", body: isAlnum, min: 32},
	{prefix: "rk_live_", kind: "stripe_key", body: isAlnum, min: 20},
	{prefix: "rk_test_", kind: "stripe_key", body: isAlnum, min: 20},
	{prefix: "whsec_", kind: "webhook_secret", body: isAlnum, min: 24},
	{prefix: "AIza", kind: "google_api_key", body: isKeyChar, min: 35, max: 35},
	{prefix: "ya29.", kind: "google_oauth_token", body: isKeyCharDot, min: 20},
	{prefix: "1//0", kind: "google_refresh_token", body: isKeyChar, min: 30},
	{prefix: "ghp_", kind: "github_token", body: isAlnum, min: 36},
	{prefix: "gho_", kind: "github_token", body: isAlnum, min: 36},
	{prefix: "ghu_", kind: "github_token", body: isAlnum, min: 36},
	{prefix: "ghs_", kind: "github_token", body: isAlnum, min: 36},
	{prefix: "ghr_", kind: "github_token", body: isAlnum, min: 36},
	{prefix: "github_pat_", kind: "github_token", body: isAlnumUnder, min: 40},
	{prefix: "glpat-", kind: "gitlab_token", body: isKeyChar, min: 20},
	{prefix: "xoxb-", kind: "slack_token", body: isAlnumDash, min: 20},
	{prefix: "xoxp-", kind: "slack_token", body: isAlnumDash, min: 20},
	{prefix: "xoxa-", kind: "slack_token", body: isAlnumDash, min: 20},
	{prefix: "xoxr-", kind: "slack_token", body: isAlnumDash, min: 20},
	{prefix: "xoxs-", kind: "slack_token", body: isAlnumDash, min: 20},
	{prefix: "xoxe-", kind: "slack_token", body: isAlnumDash, min: 20},
	{prefix: "hooks.slack.com/services/", kind: "slack_webhook", body: isAlnumSlash, min: 20},
	{prefix: "AKIA", kind: "aws_access_key_id", body: isUpperDigit, min: 16, max: 16},
	{prefix: "ASIA", kind: "aws_access_key_id", body: isUpperDigit, min: 16, max: 16},
	{prefix: "xai-", kind: "xai_key", body: isAlnum, min: 40},
	{prefix: "gsk_", kind: "groq_key", body: isAlnum, min: 40},
	{prefix: "hf_", kind: "huggingface_token", body: isAlnum, min: 30},
	{prefix: "npm_", kind: "npm_token", body: isAlnum, min: 36, max: 36},
	{prefix: "pypi-AgE", kind: "pypi_token", body: isKeyChar, min: 50},
	{prefix: "dop_v1_", kind: "digitalocean_token", body: isLowerHexOnly, min: 64, max: 64},
	{prefix: "SG.", kind: "sendgrid_key", body: isKeyCharDot, min: 60, check: isSendGridBody},
	// This relay's own panel sessions and their refresh tokens.
	{prefix: "cps_", kind: "clirelay_token", body: isKeyChar, min: 40},
	{prefix: "cpt_", kind: "clirelay_token", body: isKeyChar, min: 40},
	{prefix: "cpr_usr_", kind: "clirelay_token", body: isKeyChar, min: 40},
	{prefix: "cpr_adm_", kind: "clirelay_token", body: isKeyChar, min: 40},
}

// isSendGridBody checks the two parts of a SendGrid key: SG.<22>.<43>.
func isSendGridBody(body string) bool {
	dot := strings.IndexByte(body, '.')
	return dot == 22 && len(body)-dot-1 == 43
}

// rulesByFirstByte indexes prefixRules by the first byte of their prefix, so
// the scan looks only at the rules that can start where it stands.
var rulesByFirstByte = func() (index [256][]int) {
	for i, rule := range prefixRules {
		index[rule.prefix[0]] = append(index[rule.prefix[0]], i)
	}
	return index
}()

// formatStarts marks the first two bytes of every format, so the scan stops
// only where one can begin.
var formatStarts = func() *pairSet {
	var set pairSet
	for _, rule := range prefixRules {
		set.add(rule.prefix)
	}
	for _, start := range []string{"eyJ", "-----BEGIN ", "://"} {
		set.add(start)
	}
	return &set
}()

// scanFormats finds the credentials recognised by their own shape.
func (s *scanner) scanFormats() {
	text := s.text
	for i := 0; i+1 < len(text); i++ {
		if !formatStarts.has(text[i], text[i+1]) {
			continue
		}
		if end := s.formatAt(i); end > i {
			i = end - 1
		}
	}
}

// formatAt records a credential that starts at i and returns where it ends,
// or -1 when none starts there.
func (s *scanner) formatAt(i int) int {
	text := s.text
	switch text[i] {
	case 'e':
		if strings.HasPrefix(text[i:], "eyJ") {
			if end := s.jwt(i); end > i {
				return end
			}
		}
	case '-':
		if strings.HasPrefix(text[i:], "-----BEGIN ") {
			return s.privateKey(i)
		}
	case ':':
		if strings.HasPrefix(text[i:], "://") {
			s.urlPassword(i)
		}
		return -1
	}
	for _, index := range rulesByFirstByte[text[i]] {
		rule := &prefixRules[index]
		if strings.HasPrefix(text[i:], rule.prefix) {
			return s.prefixed(rule, i)
		}
	}
	return -1
}

func (s *scanner) prefixed(rule *prefixRule, i int) int {
	text := s.text
	if !startsToken(text, i) {
		return -1
	}
	bodyStart := i + len(rule.prefix)
	limit := rule.max
	if limit == 0 {
		limit = maxTokenLen
	}
	end := runEnd(text, bodyStart, rule.body, limit+1)
	n := end - bodyStart
	if n < rule.min || n > limit || !endsToken(text, end) {
		return -1
	}
	if rule.check != nil && !rule.check(text[bodyStart:end]) {
		return -1
	}
	s.add(i, end, rule.kind)
	return end
}

// jwt recognises header.payload.signature, both JSON parts base64url encoded.
func (s *scanner) jwt(i int) int {
	text := s.text
	if !startsToken(text, i) {
		return -1
	}
	headerEnd := runEnd(text, i, isKeyChar, maxTokenLen)
	if headerEnd-i < 10 || headerEnd >= len(text) || text[headerEnd] != '.' {
		return -1
	}
	payload := headerEnd + 1
	if !strings.HasPrefix(text[payload:], "eyJ") {
		return -1
	}
	payloadEnd := runEnd(text, payload, isKeyChar, maxTokenLen)
	if payloadEnd-payload < 10 || payloadEnd >= len(text) || text[payloadEnd] != '.' {
		return -1
	}
	signature := payloadEnd + 1
	end := runEnd(text, signature, isKeyChar, maxTokenLen)
	if end-signature < 10 || !endsToken(text, end) {
		return -1
	}
	s.add(i, end, "jwt")
	return end
}

// privateKey replaces a PEM private key from its BEGIN line through its END
// line. A key cut off before its END line (a truncated body) is replaced
// through the end of its base64 body.
func (s *scanner) privateKey(i int) int {
	text := s.text
	labelStart := i + len("-----BEGIN ")
	labelEnd := strings.Index(text[labelStart:min(len(text), labelStart+64)], "-----")
	if labelEnd < 0 || !strings.Contains(text[labelStart:labelStart+labelEnd], "PRIVATE KEY") {
		return -1
	}
	bodyStart := labelStart + labelEnd + len("-----")
	window := text[bodyStart:min(len(text), bodyStart+maxPEMLen)]
	if footer := strings.Index(window, "-----END "); footer >= 0 {
		footerLabel := bodyStart + footer + len("-----END ")
		if close := strings.Index(text[footerLabel:min(len(text), footerLabel+64)], "-----"); close >= 0 {
			end := footerLabel + close + len("-----")
			s.add(i, end, "private_key")
			return end
		}
	}
	end := pemBodyEnd(text, bodyStart)
	s.add(i, end, "private_key")
	return end
}

// pemBodyEnd returns the end of the base64 body that starts at i, line breaks
// included whether raw or escaped (\n, or \\n deeper in JSON nesting). It
// never stops inside an escape sequence.
func pemBodyEnd(text string, i int) int {
	limit := min(len(text), i+maxPEMLen)
	for i < limit {
		switch c := text[i]; {
		case isBase64Char(c) || c == ' ' || c == '\n' || c == '\r' || c == '\t':
			i++
		case c == '\\':
			j := i
			for j < limit && text[j] == '\\' {
				j++
			}
			if j < limit && (text[j] == 'n' || text[j] == 'r') {
				i = j + 1
				continue
			}
			return i
		default:
			return i
		}
	}
	return i
}

func isSchemeChar(c byte) bool { return isAlnum(c) || c == '+' || c == '.' || c == '-' }

// isAuthorityChar reports whether c can appear in the user, password or host
// of a URL as written in text, before its path.
func isAuthorityChar(c byte) bool {
	if c <= ' ' || c >= 0x7f {
		return false
	}
	switch c {
	case '/', '?', '#', '"', '\'', '\\', '<', '>', '`', '(', ')', '[', ']', '{', '}', ',':
		return false
	}
	return true
}

// urlPassword replaces the password in scheme://user:password@host, for any
// scheme: database and cache URLs, proxies, git remotes, HTTP basic auth.
func (s *scanner) urlPassword(i int) {
	text := s.text
	schemeStart := i
	for schemeStart > 0 && i-schemeStart < 32 && isSchemeChar(text[schemeStart-1]) {
		schemeStart--
	}
	if i-schemeStart < 2 {
		return
	}
	authStart := i + len("://")
	authEnd := authStart
	for authEnd < len(text) && authEnd-authStart < 512 && isAuthorityChar(text[authEnd]) {
		authEnd++
	}
	at := strings.LastIndexByte(text[authStart:authEnd], '@')
	if at < 0 {
		return
	}
	colon := strings.IndexByte(text[authStart:authStart+at], ':')
	if colon < 0 {
		return
	}
	password := text[authStart+colon+1 : authStart+at]
	if len(password) < 3 || isPlaceholder(password) {
		return
	}
	s.add(authStart+colon+1, authStart+at, "url_password")
}
