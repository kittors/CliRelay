package privacymask

import (
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// rule finds one kind of value. The match is group 1 when the pattern has
// one, else all of it; bound are the characters that may not touch it on
// either side (a key inside a longer word is no key); ok checks it further.
type rule struct {
	kind    string
	re      *regexp.Regexp
	markers []string // one of these is in any text it can match
	// fold are markers too, looked for in the text in lower case, for a
	// pattern that matches its words in any case (Passport, PASSPORT)
	fold  []string
	bound string
	ok    func(string) bool
	// at checks the value at s[a:b] by what is around it: a dotted quad
	// right after "version" is no address
	at func(s string, a, b int) bool
	// fix, where set, checks the value in place of ok and may shorten it:
	// an IBAN printed in groups of four, followed by a word in capitals
	fix func(s string, a, b int) (int, int, bool)
	// back and ahead, where ahead is set, are how far before and after
	// one of fold a match can be: the pattern is only run there, as one
	// that takes its words in any case is slow over all of a long text
	back, ahead int
	// cat is what turns the rule on (Options.covers): "" the secrets
	cat string
	// field is a rule that takes a value after a field's name and = or :,
	// a space allowed between them: a value that is the next field's
	// assignment is not this one's (fieldMatches)
	field bool
	// yours is a rule the user wrote (customRules)
	yours bool
	// anyGroup takes the first group that matched, for a pattern with a
	// group in each of its alternatives
	anyGroup bool
	// groups, where set, are the kinds of a match's groups, each masked as
	// a value of its own (the user and the host of user@host), and groupOK
	// checks each
	groups  []string
	groupOK []func(string) bool
	// needs, where set, is what a line must have for the rule to match in
	// it (lineStats): a necessary condition, cheap to check, so the
	// pattern is not run over lines it cannot match (scan.go)
	needs func(lineStats) bool
	// multiline is a rule whose match may span lines (a private key's
	// block): it is run over the whole text (scan.go)
	multiline bool
}

const (
	alnum   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	tokenCh = alnum + "_-"
	digits  = "0123456789"
)

var rules = append([]rule{
	{kind: "PRIVATE_KEY", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----[\s\S]+?-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`), markers: []string{"PRIVATE KEY"}, multiline: true},
	{kind: "API_KEY", re: regexp.MustCompile(`sk-(?:ant-|proj-|or-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`), markers: []string{"sk-"}, bound: tokenCh, ok: notASlug},
	// a gateway's or a relay's own: oc_sk_…, or_sk_… (#195)
	{kind: "API_KEY", re: regexp.MustCompile(`[a-z]{2,4}_sk_[A-Za-z0-9_-]{20,}`), markers: []string{"_sk_"}, bound: tokenCh, ok: secretValue},
	// Volcano Ark and the relays in front of it issue keys with an ark-
	// prefix (#2946)
	{kind: "API_KEY", re: regexp.MustCompile(`ark-[A-Za-z0-9][A-Za-z0-9_-]{15,}`), markers: []string{"ark-"}, bound: tokenCh, ok: secretValue},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}|glpat-[A-Za-z0-9_-]{20,})`), markers: []string{"gh", "github_pat_", "glpat-"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), markers: []string{"AIza"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`xox[abposr]-[0-9A-Za-z-]{10,}`), markers: []string{"xox"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:sk|rk)_live_[0-9A-Za-z]{20,}`), markers: []string{"_live_"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), markers: []string{"AKIA", "ASIA"}, bound: alnum},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:hf_[A-Za-z0-9]{30,}|gsk_[A-Za-z0-9]{40,}|xai-[A-Za-z0-9]{40,}|npm_[A-Za-z0-9]{36}|pypi-[A-Za-z0-9_-]{50,}|dop_v1_[a-f0-9]{64}|SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43})`),
		markers: []string{"hf_", "gsk_", "xai-", "npm_", "pypi-", "dop_v1_", "SG."}, bound: tokenCh},
	{kind: "TOKEN", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), markers: []string{"eyJ"}, bound: tokenCh},
	// the password of user:password@host, or of :password@host as a Redis
	// URL has it with no user
	{kind: "PASSWORD", re: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s:@/?#"'<>]*:([^\s@/?#"'<>]{3,})@`), markers: []string{"://"}, ok: notAVariable, at: hiddenInURL},
	// password = …, API_KEY: "…", as .env files and configs have them
	{kind: "SECRET", re: regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:` + secretNames.pattern() + `)[A-Za-z0-9_.-]*` + quote + `[ \t]*[:=][ \t]*` + quote + `(` + secretCh + `{8,})`),
		markers: secretNames.markers(), ok: secretValue, at: fieldAt, field: true},
	// an env var's name says what it holds in the upper case: a
	// SERVICE_X_KEY, a DB_PASS (#2946). The names above have a word for the
	// secret in them; these only end in one, and only in the upper case is
	// that a secret's name — in the lower case a name that ends in key is
	// code's: a sort key, a cache key, a public key (notAPlainKey turns the
	// upper-case ones of those away too).
	{kind: "SECRET", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])[A-Za-z0-9_.-]*_(?:KEY|PASS)` + quote + `[ \t]*[:=][ \t]*` + quote + `(` + secretCh + `{8,})`),
		markers: fieldNames{{"key", "key"}, {"pass", "pass"}}.markers(), ok: secretValue, at: namedSecretAt, field: true},
	// an Authorization or Cookie header's value is a secret whatever it
	// looks like: an opaque bearer token, a session value (#2946)
	{kind: "SECRET", re: regexp.MustCompile(`(?im)^[ \t]*(?:authorization|cookie)[ \t]*:[ \t]*(\S[^\r\n]{7,})`),
		markers: fieldNames{{"authorization", "authorization"}, {"cookie", "cookie"}}.markers(), ok: headerValue},
	// a field called key and nothing more, as providers.json has one: "key":
	// "oc_sk_…" (#195). Only a long value of letters and digits both, since
	// code calls anything a key: a map's "key": "value", sort_key, keyboard.
	{kind: "SECRET", re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])` + quote + `key` + quote + `[ \t]*[:=][ \t]*` + quote + `(` + secretCh + `{16,})`),
		markers: fieldNames{{"key", "key"}}.markers(), ok: secretValue, at: fieldAt, field: true},
	// a password after its name in Chinese, Japanese or Korean, as people
	// write one into a message: 密码：786kih, 密碼是 hunter2, パスワード: … .
	// Unlike password = in a config, nothing in code is called 密码, so
	// any value of four or more is taken, digits only too (CyberCaius on
	// Discord); a sentence's closing stop is no part of it.
	{kind: "PASSWORD", re: cjkNamed(cjkPasswordNames), fold: lowerAll(cjkPasswordNames), back: 64, ahead: 100, ok: notAVariable, field: true},
	// a key, a secret or a token after its name the same way: 密钥为 …,
	// 令牌是 …, 金鑰： … . A key of digits only is taken after 为, 是, ：
	// or = when it has six or more and counts nothing, so 密钥为12231241
	// is masked and 密钥为 2048 位 is left.
	{kind: "SECRET", re: cjkNamed(cjkKeyNames), fold: lowerAll(cjkKeyNames), back: 64, ahead: 100, at: keyAt, field: true},
	// an English name for one before 为, 為, 是 or ：, as a Chinese message
	// has it: apikey 为 …, token 是 …, API Key：… (CyberCaius on Discord).
	// password = … and API_KEY: … in a config are the SECRET rule's above.
	{kind: "SECRET", re: regexp.MustCompile(`(?i)(?:` + secretNames.pattern() + `|pwd|api[ ]key|access[ ]key|secret[ ]key|key|\bpin(?:[ ]?code)?)` + quote +
		`[ \t]*(?:[为為是][ \t]*[:：]?|：)[ \t]*(?:\\?["'“「])?(` + secretCh + `{3,}[A-Za-z0-9_\-/+=~!@#%^&*])`),
		fold: cjkCopulas, back: 24, ahead: 260, at: keyAt, field: true},

	{kind: "EMAIL", re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`), markers: []string{"@"}, bound: alnum + "_%+-", ok: realEmail, at: emailAt, cat: catPersonal},
	{kind: "ID_CARD", re: regexp.MustCompile(`[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]`), bound: alnum, ok: chineseID, cat: catPersonal, needs: digitRun(17)},
	{kind: "PHONE", re: regexp.MustCompile(`(?:\+?86[- ]?)?1[3-9]\d{9}`), bound: alnum, at: phoneAt, cat: catPersonal, needs: digitRun(11)},
	{kind: "BANK_CARD", re: regexp.MustCompile(`[3-6]\d{3}(?:[ -]?\d{4}){2,3}(?:[ -]?\d{1,3})?`), bound: alnum, ok: luhn, cat: catPersonal, needs: digitCount(15)},
}, append(moreRules, extraRules...)...)

// cjkPasswordNames are what a password is called in Chinese, Japanese and
// Korean.
var cjkPasswordNames = []string{"密码", "密碼", "口令", "PIN码", "PIN碼", "PIN 码", "安全码", "安全碼", "パスワード", "暗証番号", "비밀번호", "암호", "비번"}

// cjkKeyNames are what a key, a secret or a token is called in Chinese,
// Simplified and Traditional, Japanese and Korean.
var cjkKeyNames = []string{
	"密钥", "密鑰", "秘钥", "秘鑰", "私钥", "私鑰", "金钥", "金鑰", "令牌", "权杖", "權杖", "授权码", "授權碼",
	"秘密鍵", "トークン", "シークレット", "APIキー", "アクセスキー",
	"토큰", "비밀키", "API 키", "API키",
}

// cjkCopulas are what stands between a name and its value in a Chinese
// message: 为, 為, 是, a full-width colon.
var cjkCopulas = []string{"为", "為", "是", "："}

// lowerAll is names as fold has them, in lower case.
func lowerAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ToLower(n)
	}
	return out
}

// cjkNamed is a value after one of names: after ：, :, =, 为, 為, 是, は
// or 는, quoted or not, or after nothing but a space.
func cjkNamed(names []string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:` + strings.Join(names, "|") + `)[ \t]*(?:[:：=][ \t]*|[为為是][ \t]*[:：]?[ \t]*|は[ \t]*|는[ \t]*)?(?:\\?["'“「])?(` + secretCh + `{3,63}[A-Za-z0-9_\-/+=~!@#%^&*])`)
}

// fieldNames are what a field that holds a secret is called: a pattern for
// each name, and text that any match of it has in it, so a rule's markers
// and the names its pattern takes come from one list and can't drift apart.
type fieldNames []struct{ re, marker string }

// secretNames are what a field holding a secret has in its name:
// DB_PASSWORD, apiKey, x-api-key, client_secret.
var secretNames = fieldNames{
	{"password", "pass"}, {"passwd", "pass"},
	{"secret", "secret"},
	{"token", "token"},
	{`api[_-]?key`, "key"}, {`access[_-]?key`, "key"}, {`private[_-]?key`, "key"},
	{"credential", "credential"},
}

func (f fieldNames) pattern() string {
	res := make([]string, len(f))
	for i, n := range f {
		res[i] = n.re
	}
	return strings.Join(res, "|")
}

// markers are each name's marker as a name is written in lower case, in
// upper case and capitalised: password, PASSWORD, Password, apiKey.
func (f fieldNames) markers() []string {
	var out []string
	for _, n := range f {
		for _, m := range []string{n.marker, strings.ToUpper(n.marker), strings.ToUpper(n.marker[:1]) + n.marker[1:]} {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

const (
	// quote is a field's or a value's quote, if it has one: " or ', or \"
	// in JSON written inside a string
	quote = `(?:\\?["'])?`
	// secretCh are the characters of a secret's value
	secretCh = `[A-Za-z0-9_\-./+=~!@#%^&*]`
)

// notAVariable turns away what stands in for a value: ${PASS}, <password>, ****.
func notAVariable(v string) bool {
	if strings.ContainsAny(v[:1], "$%{<[*") {
		return false
	}
	return strings.Trim(v, "*xX.") != ""
}

// keyValue is a key's or a token's value, not a word or a number: letters
// and digits both, or 16 characters or more that aren't all digits. token 是
// expired, key 是 value and 密钥为 2048 are left.
func keyValue(v string) bool {
	if !notAVariable(v) {
		return false
	}
	letter := strings.IndexFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0
	return letter && (strings.ContainsAny(v, digits) || len(v) >= 16)
}

// keyAt is keyValue for s[a:b], or six digits or more right after 为, 為,
// 是, ：, :, =, は or 는 that count nothing: a key said in a message is
// often a number (密钥为12231241, token 是 20261025, CyberCaius on
// Discord), while 密钥为 2048 位 and 令牌 300000 毫秒 are a size and a
// time.
func keyAt(s string, a, b int) bool {
	v := s[a:b]
	if pathValue(v) || versionValue(v) {
		// where a key is kept: 私钥 ~/.ssh/id_ed25519, or a release's
		// version: token 是 0.15.0-rc1 (fieldvalue.go)
		return false
	}
	if keyValue(v) {
		return true
	}
	if len(v) < 6 || strings.Trim(v, digits) != "" {
		return false
	}
	before := strings.TrimRight(s[:a], " \t\"'“「\\")
	after := strings.TrimLeft(s[b:], " \t")
	return slices.ContainsFunc(keySeparators, func(c string) bool { return strings.HasSuffix(before, c) }) &&
		!slices.ContainsFunc(quantityUnits, func(u string) bool { return strings.HasPrefix(after, u) })
}

// keySeparators are what stands between a name and a key's value.
var keySeparators = []string{"为", "為", "是", "：", ":", "=", "は", "는"}

// quantityUnits are what a number counts when one follows it: bits, bytes,
// a time, a count.
var quantityUnits = []string{"位", "比特", "字节", "字節", "个", "個", "次", "秒", "毫秒", "分钟", "分鐘", "小时", "小時", "天", "年", "bit", "byte", "ms", "sec", "min", "ビット", "バイト", "비트", "바이트"}

// secretValue is a value that looks like a secret, not code: letters and
// digits both, and not a call, a reference or a placeholder of its own.
func secretValue(v string) bool {
	if !notAVariable(v) || strings.HasPrefix(v, "process.env") || strings.HasPrefix(v, "os.") {
		return false
	}
	return strings.ContainsAny(v, digits) && strings.IndexFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0
}

// identPath is a name in code, or a field read through one: Uint8Array,
// details.2, opts.token2.
var identPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)*$`)

// notCode turns away a value at s[a:b] that is code naming a secret, not
// the secret: a call (privateKey: toBase64Url(…), secretKey =
// fromBase64Url(…)), or, unquoted after a colon, a type or a read that
// code goes on from (secretKey: Uint8Array;, private_key: details.2,).
// A secret written in code is a string, so in quotes; one after = or at a
// line's end (.env files, logs, YAML) is kept (Kieran Zhang on X, 10-25).
func notCode(s string, a, b int) bool {
	if !identPath.MatchString(s[a:b]) {
		return true
	}
	rest := strings.TrimLeft(s[b:], " \t")
	if strings.HasPrefix(rest, "(") {
		return false
	}
	if a > 0 && (s[a-1] == '"' || s[a-1] == '\'') || !strings.HasSuffix(strings.TrimRight(s[:a], " \t"), ":") {
		return true
	}
	return rest == "" || !strings.ContainsRune(";,)}]|?&<>", rune(rest[0]))
}

// notUserinfo turns away what looks like an email at s[a:b] but is a
// URL's user:password@host or user@host: its @ ends the userinfo, so
// what is before it is a user name or a password and what is after it
// the host. postgres://app:hunter2pass@db.acme-corp.cn:5432/x went as
// app:{{EMAIL_…}}:5432, the password and the host one address, where the
// password is the PASSWORD rule's and the host stays (#3538). An address
// in a URL's path or query (?to=bob@acme.cn) and in mailto: is an
// email still.
func notUserinfo(s string, a, _ int) bool {
	return !inUserinfo(s, a)
}

// emailAt is where an email at s[a:b] can be: not in a URL's userinfo
// (notUserinfo), and with no dot touching it that makes it part of a
// longer name: none before it, and none after it that a letter, digit,
// - or _ follows (a@b.io.12 is no address). A dot after it that ends a
// sentence is the sentence's: "The user's email address is
// jo@quill.dev." masked nothing while the dot was a bound character, and
// the address went to the vendor as written.
func emailAt(s string, a, b int) bool {
	if a > 0 && s[a-1] == '.' {
		return false
	}
	if b+1 < len(s) && s[b] == '.' && strings.IndexByte(tokenCh, s[b+1]) >= 0 {
		return false
	}
	return notUserinfo(s, a, b)
}

// inUserinfo says s[a] is inside a URL's authority, after its scheme://
// and before the host: nothing from there back to the // is a /, ?, #,
// space, quote, < or > or @.
func inUserinfo(s string, a int) bool {
	i := a
	for i > 0 && !strings.ContainsRune(" \t\r\n/?#\"'<>@`", rune(s[i-1])) {
		i--
	}
	if i < 3 || s[i-1] != '/' || s[i-2] != '/' || s[i-3] != ':' {
		return false
	}
	j := i - 3 // the scheme ends at s[j], its colon
	k := j
	for k > 0 && (s[k-1] >= 'a' && s[k-1] <= 'z' || s[k-1] >= 'A' && s[k-1] <= 'Z' || s[k-1] >= '0' && s[k-1] <= '9' || strings.IndexByte("+.-", s[k-1]) >= 0) {
		k--
	}
	for k < j && !(s[k] >= 'a' && s[k] <= 'z' || s[k] >= 'A' && s[k] <= 'Z') {
		k++ // a scheme starts with a letter
	}
	return k < j
}

// hiddenInURL says the password at s[a:b], in a URL's user:password@,
// is hidden by masking it: it is not also written as it is elsewhere in
// the same URL, as its user name, a part of its host or of its path. A
// test database's URL often gives one word for all three:
// postgresql://geodatafusion:geodatafusion@127.0.0.1:55432/geodatafusion
// is the PostGIS test of the geodatafusion repo. Its password was masked
// while the URL still had it twice as written, and once masked the word
// was known (knownSpans): the project's folders, its crate, its .toml
// and cargo test -p geodatafusion went to the vendor as a made-up word
// for the rest of the run, as a password (xuanwo on X, 10-29: "很多项目名
// 被识别成了 password", 26M masked in a day). postgres:postgres,
// root:root and guest:guest are the same: anyone who reads the user
// reads the password. A password not written elsewhere in its URL is
// masked as before, whatever it looks like: app:geodatafusion@ masks it
// there and, as known, wherever it is next.
func hiddenInURL(s string, a, b int) bool {
	pw := s[a:b]
	i := strings.LastIndex(s[:a], "//")
	if i < 0 || s[a-1] != ':' {
		return true
	}
	if s[i+2:a-1] == pw {
		return false // the user name
	}
	// the host and the path, to the URL's end
	end := b + 1
	for end < len(s) && !strings.ContainsRune(" \t\r\n\"'<>`\\)]}", rune(s[end])) {
		end++
	}
	if b+1 > end {
		return true
	}
	for _, part := range strings.FieldsFunc(s[b+1:end], func(r rune) bool { return strings.ContainsRune("/:.?#&=;,@", r) }) {
		if part == pw {
			return false
		}
	}
	return true
}

// realEmail is whether v, matched as an address, is someone's. git@ is
// the user every git host is reached by over SSH (git@github.com:o/r.git,
// git@gitee.com, a company's own git@git.corp.com), no one's mailbox, and
// was masked so a remote no longer read as one (jumy on Discord).
// notASlug is false for words in lower case joined by hyphens, as a
// page's address names an article about SK hynix
// (cnbc.com/…/sk-hynix-third-quarter-profit.html), which went to the model
// as a key. A key's characters are random: it has a capital, or a run of
// 16 or more with no hyphen (DeepSeek's sk- and 32 hex digits, OpenRouter's
// sk-or-v1- and 64), or hex digits alone, as a UUID's groups (sk- and a
// UUID in lower case) are, which no word but a few like "face" is.
func notASlug(v string) bool {
	if strings.ContainsFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' }) {
		return true
	}
	if !strings.ContainsFunc(strings.TrimPrefix(v, "sk-"), func(r rune) bool {
		return r != '-' && r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) {
		return true
	}
	for _, w := range strings.FieldsFunc(v, func(r rune) bool { return r == '-' || r == '_' }) {
		if len(w) >= 16 {
			return true
		}
	}
	return false
}

// headerValue says the rest of an Authorization or Cookie line is a
// header's value, not prose that begins with the word ("Authorization:
// the user must sign in first"): a credential, after a scheme word or
// alone (Bearer …, Basic …), or a cookie's name=value pairs. One that
// stands for a variable (${TOKEN}, <token>, ****) is left alone.
func headerValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || !notAVariable(v) {
		return false
	}
	notLetter := func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') }
	f := strings.Fields(v)
	if len(f) == 1 || len(f) == 2 && strings.IndexFunc(f[0], notLetter) < 0 {
		// a word alone is prose's: a credential has a digit or a sign in it
		cred := f[len(f)-1]
		return len(cred) >= 8 && notAVariable(cred) && strings.IndexFunc(cred, notLetter) >= 0
	}
	for _, kv := range strings.Split(v, ";") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return false
		}
	}
	return strings.Contains(v, "=")
}

// notAPlainKey turns away the upper-case name before the value at s[:a]
// when it only ends in key and holds no secret: a sort key, a cache key, a
// prompt cache key, a public key (#2946).
func notAPlainKey(s string, a, b int) bool {
	i := a
	for i > 0 && strings.IndexByte(`"' `+"\t:=", s[i-1]) >= 0 {
		i--
	}
	end := i
	for i > 0 && strings.IndexByte(alnum+"_.-", s[i-1]) >= 0 {
		i--
	}
	name := strings.ToLower(s[i:end])
	return !strings.HasSuffix(name, "sort_key") && !strings.HasSuffix(name, "cache_key") &&
		!strings.HasSuffix(name, "prompt_cache_key") && !strings.HasSuffix(name, "public_key")
}

// notPlainKeyOrCode keeps both boundaries on a name that only ends in _KEY
// or _PASS: plain keys stay as written, and so does code naming a secret.
func notPlainKeyOrCode(s string, a, b int) bool {
	return notAPlainKey(s, a, b) && notCode(s, a, b)
}

// namedSecretAt is notPlainKeyOrCode with the value-side reading the SECRET
// rules' fieldAt gained later: a const TOKEN_KEY = "access_token_v2" is
// code naming a token, and its value reads as a version, so both stay.
func namedSecretAt(s string, a, b int) bool {
	return notPlainKeyOrCode(s, a, b) && fieldAt(s, a, b)
}

func realEmail(v string) bool {
	at := strings.LastIndexByte(v, '@')
	if strings.EqualFold(v[:at], "git") {
		return false
	}
	host := strings.ToLower(v[at+1:])
	if !mailHost(v[:at], host) {
		return false
	}
	if fileHost(host) || docFiles[host] || !mailTLD(host[strings.LastIndexByte(host, '.')+1:]) {
		return false
	}
	for _, fake := range []string{"example.com", "example.org", "example.net", "localhost", ".test", ".invalid", ".example"} {
		if host == strings.TrimPrefix(fake, ".") || strings.HasSuffix(host, fake) {
			return false
		}
	}
	return !strings.HasPrefix(host, "noreply") && !strings.Contains(v, "noreply@")
}

// mailHost is whether host, after local and an @, can be a mail domain:
// it has a dot, labels of letters, digits and hyphens (or a name's own
// script's: 例子.中国), and is not a
// linker's table; and local has no @ of its own. A disassembly's symbols
// read as addresses: system@plt, puts@got.plt, a version after one @ or
// two (memcpy@GLIBC_2.14, foo@@GLIBC_2.2.5, foo@Base). .got is a
// top-level domain, and the local model took system@got for an email, so
// a reverse engineer had to turn masking off (ch35tnut_ on X, 10-28).
func mailHost(local, host string) bool {
	if strings.HasSuffix(local, "@") || !strings.Contains(host, ".") || strings.Trim(host, ".") != host {
		return false
	}
	linker := true
	for _, l := range strings.Split(host, ".") {
		if l == "" || strings.ContainsFunc(l, func(r rune) bool { return r < 0x80 && r != '-' && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') }) {
			return false
		}
		if !linkerWords[l] {
			linker = false
		}
	}
	return !linker
}

// linkerWords are the names of a binary's tables a symbol is reached
// through (system@plt, puts@got.plt, foo@plt.sec), no domain's labels.
var linkerWords = map[string]bool{"plt": true, "got": true, "sec": true, "iplt": true, "igot": true}

// docFiles are the files agents read by name, on a domain's ending
// (.md): "@AGENTS.md" after a word, as a prompt names one, is no address
// (ydzh on Discord: xxx@Agents.md was masked).
var docFiles = map[string]bool{
	"agents.md": true, "claude.md": true, "gemini.md": true, "readme.md": true, "skill.md": true,
	"changelog.md": true, "contributing.md": true, "license.md": true, "lessons.md": true, "todo.md": true,
	"copilot-instructions.md": true, "crush.md": true, "qwen.md": true, "iflow.md": true, "warp.md": true,
}

// localTLDs are names an organisation's own mail can end in that no
// registry has: an address there is someone's all the same.
var localTLDs = map[string]bool{"local": true, "internal": true, "lan": true, "corp": true, "home": true, "intranet": true, "localdomain": true}

// mailTLD is whether tld, an address's last label, is one mail can be
// sent to: in the public suffix list's ICANN part, or an organisation's
// own. A decorator's or a module's attribute after a name
// (x@pytest.fixture, ydzh on Discord) ends in a word no registry has.
func mailTLD(tld string) bool {
	if localTLDs[tld] {
		return true
	}
	_, icann := publicsuffix.PublicSuffix(tld)
	return icann
}

// fileExt are file name extensions that are no top-level domain, so
// what ends in one is a file's name and not an address: an image a Mac
// saved at twice its size, 261020_14-16-11@2x.jpg, went to the model as
// {{EMAIL_…}} and Pi could no longer read the file (#3029).
// Other files are named so too (intro@2x.mp4, strings@base.json), and
// none of these is a domain either (IANA's list, checked 2026-10-21).
var fileExt = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "svg": true, "heic": true, "heif": true,
	"avif": true, "bmp": true, "tif": true, "tiff": true, "ico": true, "icns": true, "pdf": true,
	"svgz": true, "apng": true, "jxl": true, "psd": true, "eps": true, "lottie": true,
	"mp4": true, "m4v": true, "webm": true, "mkv": true, "avi": true, "mp3": true, "m4a": true, "wav": true,
	"ogg": true, "flac": true, "aac": true, "ttf": true, "otf": true, "woff": true, "woff2": true, "eot": true,
	"json": true, "yaml": true, "yml": true, "toml": true, "xml": true, "txt": true, "csv": true, "tsv": true,
	"log": true, "html": true, "htm": true, "css": true, "scss": true, "less": true, "js": true, "mjs": true,
	"cjs": true, "jsx": true, "ts": true, "tsx": true, "doc": true, "docx": true, "xls": true, "xlsx": true,
	"ppt": true, "pptx": true, "gz": true, "tgz": true, "tar": true, "rar": true, "7z": true, "dmg": true,
}

// scaleExt are file extensions that are top-level domains too (.zip,
// .mov, .md, .map): a name ending in one is a file's only with a scale
// before it, icon@2x.zip, intro@1.5x.mov, while ops@files.zip is an
// address.
var scaleExt = map[string]bool{"zip": true, "mov": true, "md": true, "map": true}

var scaleRe = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?x$`)

// fileHost says host, what follows the @ of what looks like an address,
// is the end of a file's name: an extension no domain has (fileExt), or a
// scale and an extension that is a domain too (scaleExt).
func fileHost(host string) bool {
	dot := strings.LastIndexByte(host, '.')
	ext := host[dot+1:]
	return fileExt[ext] || dot > 0 && scaleExt[ext] && scaleRe.MatchString(host[:dot])
}

func chineseID(v string) bool {
	w := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(v[i]-'0') * w[i]
	}
	return strings.ToUpper(v[17:]) == string("10X98765432"[sum%11])
}

func luhn(v string) bool {
	var ds []int
	for _, r := range v {
		if r >= '0' && r <= '9' {
			ds = append(ds, int(r-'0'))
		}
	}
	if len(ds) < 15 || len(ds) > 19 {
		return false
	}
	sum := 0
	for i := range ds {
		d := ds[len(ds)-1-i]
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
