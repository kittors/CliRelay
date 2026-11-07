package secretscrub

import (
	"encoding/base64"
	"strings"
)

// maxNameLen bounds the field name examined before a value.
const maxNameLen = 128

// maxFieldLen bounds a field value; a header line or a quoted value can be
// long (a cookie jar), a bare value rarely is.
const (
	maxFieldLen = 64 << 10
	maxBareLen  = 1 << 10
)

// nameClass is what a field's name says its value holds.
type nameClass int

const (
	nameNone nameClass = iota
	namePassword
	nameSecret
	nameAuthorization
	nameCookie
)

func (c nameClass) kind() string {
	switch c {
	case namePassword:
		return "password"
	case nameAuthorization:
		return "authorization"
	case nameCookie:
		return "cookie"
	}
	return "secret"
}

// The names are matched on their end, after lower-casing and dropping '_',
// '-' and '.': DB_PASSWORD, client-secret, x-goog-api-key, AWS.SecretAccessKey.
// Only whole suffixes count, so max_tokens, sort_key and token_count are not
// taken for credentials.
var (
	passwordSuffixes = []string{"password", "passwd", "passphrase", "passcode"}
	secretSuffixes   = []string{
		"secret", "token", "apikey", "accesskey", "secretkey", "privatekey", "authkey",
		"signingkey", "masterkey", "encryptionkey", "accountkey", "credential", "credentials",
	}
)

// scanNames finds credentials by what introduces them: a field or variable
// whose name says it holds one, an Authorization scheme, or a phrase such as
// 密码：.
func (s *scanner) scanNames() {
	text := s.text
	examined := -1 // end of the last field name examined
	for i := 0; i+1 < len(text); i++ {
		c := text[i]
		if !nameStarts.has(c, text[i+1]) {
			continue
		}
		switch lowerASCII(c) {
		case 'p', 's', 't', 'k', 'c', 'a':
			if i >= examined {
				if end := s.fieldAt(i); end > i {
					examined = end
				}
			}
		case 'b':
			s.authScheme(i)
		}
		if c >= 0xe3 && c <= 0xeb {
			s.phraseAt(i)
		}
	}
}

// nameStarts marks the first two bytes of the words scanNames looks for, in
// any case: the anchor words, Bearer and Basic, and the CJK phrases.
var nameStarts = func() *pairSet {
	var set pairSet
	for _, word := range []string{"pass", "secret", "token", "key", "credential", "cookie", "authorization", "bearer ", "basic "} {
		set.addFold(word)
	}
	for _, anchor := range phraseAnchors {
		set.add(anchor.word)
	}
	return &set
}()

// anchorAt reports whether one of the words that credential names end with
// starts at i.
func anchorAt(text string, i int) bool {
	rest := text[i:]
	switch lowerASCII(text[i]) {
	case 'p':
		return hasPrefixFold(rest, "pass")
	case 's':
		return hasPrefixFold(rest, "secret")
	case 't':
		return hasPrefixFold(rest, "token")
	case 'k':
		return hasPrefixFold(rest, "key")
	case 'c':
		return hasPrefixFold(rest, "credential") || hasPrefixFold(rest, "cookie")
	case 'a':
		return hasPrefixFold(rest, "authorization")
	}
	return false
}

func isNameChar(c byte) bool { return isKeyChar(c) || c == '.' }

// fieldAt examines the field name around an anchor word at i and replaces
// the value assigned to it when the name is a credential's. It returns the
// end of the name, or -1 when no anchor word starts at i.
func (s *scanner) fieldAt(i int) int {
	text := s.text
	if !anchorAt(text, i) {
		return -1
	}
	start := i
	for start > 0 && i-start < maxNameLen && isNameChar(text[start-1]) {
		// The letter of an escape such as \n before a name is not part of it,
		// and neither is a run of dashes (--password, -----END …-----Name).
		if start >= 2 && (text[start-2] == '\\' && isEscapeLetter(text[start-1]) || text[start-2] == '-' && text[start-1] == '-') {
			break
		}
		start--
	}
	end := i
	for end < len(text) && end-start < maxNameLen && isNameChar(text[end]) {
		if text[end] == '-' && end+1 < len(text) && text[end+1] == '-' {
			break
		}
		end++
	}
	if class := classifyName(text[start:end]); class != nameNone {
		s.fieldValue(end, class)
	}
	return end
}

// classifyName says what a field called name holds.
func classifyName(name string) nameClass {
	var buf [maxNameLen]byte
	n := 0
	for i := 0; i < len(name) && n < len(buf); i++ {
		switch c := name[i]; c {
		case '_', '-', '.':
		default:
			buf[n] = lowerASCII(c)
			n++
		}
	}
	norm := string(buf[:n])
	switch norm {
	case "authorization", "proxyauthorization":
		return nameAuthorization
	case "cookie", "setcookie":
		return nameCookie
	}
	for _, suffix := range passwordSuffixes {
		if strings.HasSuffix(norm, suffix) {
			return namePassword
		}
	}
	for _, suffix := range secretSuffixes {
		if strings.HasSuffix(norm, suffix) {
			return nameSecret
		}
	}
	return nameNone
}

// quoteAt returns the quote that starts at k, escaped any number of times
// (", \", \\\" …), and where it ends; 0 when there is none.
func quoteAt(text string, k int) (byte, int) {
	j := k
	for j < len(text) && text[j] == '\\' {
		j++
	}
	if j < len(text) && (text[j] == '"' || text[j] == '\'') {
		return text[j], j + 1
	}
	return 0, k
}

func skipBlanks(text string, k int) int {
	for k < len(text) && (text[k] == ' ' || text[k] == '\t') {
		k++
	}
	return k
}

// isBareValueChar reports whether c can be part of an unquoted value: no
// space, quote, backslash or the punctuation that ends a value in code, a
// query string or a shell command.
func isBareValueChar(c byte) bool {
	if c <= ' ' || c >= 0x7f {
		return false
	}
	switch c {
	case '"', '\'', '\\', '`', ',', ';', '&', '<', '>', '(', ')', '{', '}', '[', ']':
		return false
	}
	return true
}

// isQuotedValueChar reports whether c can be part of a quoted value. A
// backslash ends it too: what follows is an escape, possibly the closing
// quote of a nested document.
func isQuotedValueChar(c byte) bool {
	return c >= ' ' && c != 0x7f && c != '"' && c != '\'' && c != '\\'
}

// isHeaderValueChar reports whether c can be part of a header's value, which
// runs to the end of its line.
func isHeaderValueChar(c byte) bool {
	return c >= ' ' && c != 0x7f && c != '"' && c != '\\'
}

// fieldValue replaces the value assigned after a credential's name ending at
// nameEnd: name=value, name: value, "name": "value", and the same inside
// JSON strings, where the quotes are escaped.
func (s *scanner) fieldValue(nameEnd int, class nameClass) {
	text := s.text
	keyQuote, k := quoteAt(text, nameEnd)
	k = skipBlanks(text, k)
	if k >= len(text) || (text[k] != ':' && text[k] != '=') {
		return
	}
	k++
	if k < len(text) && text[k] == '=' && text[k-1] == ':' {
		k++
	}
	k = skipBlanks(text, k)
	quote, k := quoteAt(text, k)
	if keyQuote != 0 && quote == 0 {
		// A quoted name with an unquoted value is a JSON member holding a
		// number, a literal, an array or an object: replacing it would leave
		// invalid JSON behind, at whatever depth the document is nested.
		return
	}
	var end int
	switch {
	case quote != 0:
		end = runEnd(text, k, isQuotedValueChar, maxFieldLen)
		if startsWithJSONStructure(text[k:end]) {
			return
		}
	case class == nameAuthorization || class == nameCookie:
		end = runEnd(text, k, isHeaderValueChar, maxFieldLen)
	default:
		end = runEnd(text, k, isBareValueChar, maxBareLen)
	}
	if class == nameAuthorization {
		k = skipAuthScheme(text, k, end)
	}
	var next byte
	if end < len(text) {
		next = text[end]
	}
	if plausibleValue(class, text[k:end], next) {
		s.add(k, end, class.kind())
	}
}

// maxSchemeLen fits the longest scheme in use, AWS4-HMAC-SHA256.
const maxSchemeLen = 20

// skipAuthScheme returns where the credential starts in an Authorization
// value running from k to end: after its scheme (Bearer, Basic, Digest,
// AWS4-HMAC-SHA256, …) and the blank that follows it. Which scheme a client
// used helps troubleshooting and says nothing about the credential. A value
// without a scheme starts at k.
func skipAuthScheme(text string, k, end int) int {
	j := k
	for j < end && j-k <= maxSchemeLen && (isAlnum(text[j]) || text[j] == '-') {
		j++
	}
	if j == k || j-k > maxSchemeLen || j >= end || text[j] != ' ' {
		return k
	}
	for j < end && (text[j] == ' ' || text[j] == '\t') {
		j++
	}
	return j
}

// startsWithJSONStructure reports whether a quoted value begins the way the
// text after a closing JSON quote must: blanks, then one of , : ] }, or
// nothing. The quote taken for the value's opening one then closed the string
// the name sits in ({"note":"Cookie:"}, ["token:", 1]), and what follows is
// the document's structure, which must not be replaced.
func startsWithJSONStructure(value string) bool {
	value = strings.TrimLeft(value, " \t\r\n")
	return value == "" || strings.IndexByte(",:]}", value[0]) >= 0
}

// plausibleValue reports whether value, assigned to a field of class, looks
// like a credential rather than code, a type, a placeholder or a setting.
func plausibleValue(class nameClass, value string, next byte) bool {
	value = strings.TrimRight(value, " \t")
	if value == "" || isRedacted(value) {
		return false
	}
	if class == nameAuthorization || class == nameCookie {
		// The header is replaced whatever it holds, a masked value included:
		// persisted request details never keep any part of a credential.
		return true
	}
	if next == '(' || isPlaceholder(value) || isCodeReference(value) {
		return false
	}
	letters, digits, symbols, upper, lower := charClasses(value)
	switch class {
	case namePassword:
		if len(value) < 4 {
			return false
		}
		if digits > 0 || symbols > 0 {
			return true
		}
		// Letters alone: a word such as supersecret, not an identifier.
		return len(value) >= 8 && !looksLikeIdentifier(value)
	default:
		if len(value) < 8 {
			return false
		}
		return letters > 0 && digits > 0 || symbols > 0 || len(value) >= 24 && upper > 0 && lower > 0
	}
}

// charClasses counts value's letters, digits and symbols. '_' and '-' are not
// counted as symbols: they join words in identifiers and names.
func charClasses(value string) (letters, digits, symbols, upper, lower int) {
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c >= 'a' && c <= 'z':
			letters++
			lower++
		case c >= 'A' && c <= 'Z':
			letters++
			upper++
		case c >= '0' && c <= '9':
			digits++
		case c == '_' || c == '-' || c == ' ':
		default:
			symbols++
		}
	}
	return
}

// looksLikeIdentifier reports whether letters-only value is written like a
// variable: snake_case or camelCase.
func looksLikeIdentifier(value string) bool {
	if strings.IndexByte(value, '_') >= 0 {
		return true
	}
	for i := 1; i < len(value); i++ {
		if value[i] >= 'A' && value[i] <= 'Z' && value[i-1] >= 'a' && value[i-1] <= 'z' {
			return true
		}
	}
	return false
}

func isRedacted(value string) bool {
	return strings.HasPrefix(value, "[REDACTED") || strings.EqualFold(value, "<redacted>")
}

// placeholderWords are values that stand for a credential in docs, schemas
// and type annotations rather than being one.
var placeholderWords = map[string]bool{
	"null": true, "none": true, "nil": true, "undefined": true, "true": true, "false": true,
	"string": true, "str": true, "bool": true, "boolean": true, "int": true, "integer": true,
	"number": true, "any": true, "object": true, "optional": true, "required": true,
	"secret": true, "secrets": true, "password": true, "passwd": true, "token": true, "key": true,
	"apikey": true, "api_key": true, "changeme": true, "redacted": true, "dummy": true,
	"test": true, "include": true, "omit": true, "same-origin": true, "hidden": true,
	"text": true, "env": true, "default": true, "empty": true, "unset": true, "value": true,
	"todo": true, "tbd": true, "n/a": true, "secretstr": true, "sensitive": true,
}

// isPlaceholder reports whether value stands for a credential without being
// one: a filler such as **** or xxxx, a masked value such as sk-a…wxyz, a
// template, or a word such as "string" or "changeme".
func isPlaceholder(value string) bool {
	if isFiller(value) || strings.Contains(value, "...") || strings.Contains(value, "…") {
		return true
	}
	switch value[0] {
	case '$', '%', '{', '<', '[':
		return true
	}
	lower := strings.ToLower(value)
	if placeholderWords[lower] {
		return true
	}
	for _, marker := range []string{"example", "placeholder", "changeme", "your_", "your-", "yourpass", "yourkey", "yoursecret", "yourtoken"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isFiller reports whether value repeats one character, or is made only of
// the characters used to blank a value out.
func isFiller(value string) bool {
	same := true
	blank := true
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c != value[0] {
			same = false
		}
		switch c {
		case 'x', 'X', '*', '#', '.', '-', '_':
		default:
			blank = false
		}
	}
	return same || blank
}

// isCodeReference reports whether value refers to a credential instead of
// holding it: config.password, os.environ, process.env.TOKEN.
func isCodeReference(value string) bool {
	segments := 0
	start := 0
	for i := 0; i <= len(value); i++ {
		if i < len(value) && value[i] != '.' {
			continue
		}
		segment := value[start:i]
		if segment == "" || !isIdentifierSegment(segment) {
			return false
		}
		segments++
		start = i + 1
	}
	return segments >= 2
}

func isIdentifierSegment(segment string) bool {
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func isBearerChar(c byte) bool {
	return isAlnum(c) || c == '-' || c == '.' || c == '_' || c == '~' || c == '+' || c == '/' || c == '='
}

// authScheme replaces the credential after "Bearer " or "Basic " at i, as
// written in a curl command, a header or a code sample.
func (s *scanner) authScheme(i int) {
	text := s.text
	var after int
	bearer := false
	switch {
	case hasPrefixFold(text[i:], "bearer "):
		after, bearer = i+len("bearer "), true
	case hasPrefixFold(text[i:], "basic "):
		after = i + len("basic ")
	default:
		return
	}
	if i > 0 && isAlnum(text[i-1]) && !(i >= 2 && text[i-2] == '\\' && isEscapeLetter(text[i-1])) {
		return
	}
	after = skipBlanks(text, after)
	if bearer {
		end := runEnd(text, after, isBearerChar, maxTokenLen)
		value := text[after:end]
		letters, digits, _, _, _ := charClasses(value)
		if len(value) >= 16 && letters > 0 && digits > 0 && !isPlaceholder(value) {
			s.add(after, end, "bearer_token")
		}
		return
	}
	end := runEnd(text, after, isBase64Char, 1024)
	if isBasicCredential(text[after:end]) {
		s.add(after, end, "basic_auth")
	}
}

// isBasicCredential reports whether value is base64 for user:password.
func isBasicCredential(value string) bool {
	if len(value) < 8 {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		if decoded, err = base64.RawStdEncoding.DecodeString(value); err != nil {
			return false
		}
	}
	colon := -1
	for i, c := range decoded {
		if c < ' ' || c >= 0x7f {
			return false
		}
		if c == ':' && colon < 0 {
			colon = i
		}
	}
	return colon > 0 && colon < len(decoded)-1
}

// phraseAnchors introduce a password or key in Chinese, Japanese and Korean
// prose: 密码：…, 密钥是…, 令牌为…, パスワードは…, 비밀번호는….
var phraseAnchors = []struct {
	word string
	kind string
}{
	{"密码", "password"}, {"密碼", "password"}, {"口令", "password"},
	{"密钥", "secret"}, {"密鑰", "secret"}, {"秘钥", "secret"}, {"秘鑰", "secret"}, {"令牌", "secret"},
	{"パスワード", "password"}, {"비밀번호", "password"},
}

// phraseConnectors join an anchor and its value.
var phraseConnectors = []string{"：", ":", "=", "是", "为", "為", "は", "는", "은"}

// phraseOpeners may stand between the connector and the value.
var phraseOpeners = []string{" ", "\t", "　", "“", "「", "『", "：", ":"}

func (s *scanner) phraseAt(i int) {
	text := s.text
	kind := ""
	k := i
	for _, anchor := range phraseAnchors {
		if strings.HasPrefix(text[i:], anchor.word) {
			kind, k = anchor.kind, i+len(anchor.word)
			break
		}
	}
	if kind == "" {
		return
	}
	k = skipPhrase(text, k, []string{" ", "\t", "　"})
	connected := false
	for _, connector := range phraseConnectors {
		if strings.HasPrefix(text[k:], connector) {
			k += len(connector)
			connected = true
			break
		}
	}
	if !connected {
		return
	}
	k = skipPhrase(text, k, phraseOpeners)
	if _, after := quoteAt(text, k); after > k {
		k = after
	}
	end := runEnd(text, k, isBareValueChar, maxBareLen)
	for end > k && strings.IndexByte(".!?)]}:", text[end-1]) >= 0 {
		end--
	}
	value := text[k:end]
	letters, digits, _, _, _ := charClasses(value)
	if len(value) >= 4 && letters+digits > 0 && !isPlaceholder(value) && !isCodeReference(value) {
		s.add(k, end, kind)
	}
}

// skipPhrase skips any of words at k, repeatedly.
func skipPhrase(text string, k int, words []string) int {
	for moved := true; moved; {
		moved = false
		for _, word := range words {
			if strings.HasPrefix(text[k:], word) {
				k += len(word)
				moved = true
			}
		}
	}
	return k
}
