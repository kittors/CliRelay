// Package clientident names the application that sent a request to the gateway
// (Codex Desktop, Claude Code, curl ...) from its User-Agent and the few headers
// some clients set instead. The request log stores the result so the panel can
// show and filter requests by caller.
//
// It only labels requests. Routing, identity fingerprints and upstream
// impersonation never read it (identityfingerprint has its own, stricter
// parsing), so a client that misreports itself only mislabels its own rows.
package clientident

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

// Client is the caller of one request.
type Client struct {
	// Name is one of the names below for a client this package recognizes, and
	// otherwise the first product token of the User-Agent in lower case
	// ("okhttp", "python-requests"). It is empty when the request carried no
	// User-Agent and no client header, or when the product token has no
	// character a name can keep.
	Name string
	// Version is the version the client reported for itself, empty when it
	// reported none.
	Version string
	// UserAgent is the raw header with control characters replaced, cut to
	// MaxUserAgentBytes.
	UserAgent string
}

// MaxUserAgentBytes bounds the stored User-Agent. Real clients stay well under
// it (Codex Desktop sends about 110 bytes); the header is caller-controlled, and
// every request writes it into the log.
const MaxUserAgentBytes = 256

const (
	maxNameLen    = 40
	maxVersionLen = 48
)

// Names of the clients this package recognizes. The panel keys its labels and
// icons on them, so a name is never changed once it has shipped.
const (
	CodexDesktop   = "codex-desktop"
	CodexCLI       = "codex-cli"
	CodexExec      = "codex-exec"
	CodexIDE       = "codex-vscode"
	CodexSDK       = "codex-sdk"
	ClaudeCode     = "claude-code"
	ClaudeAgentSDK = "claude-agent-sdk"
	GeminiCLI      = "gemini-cli"
	QwenCode       = "qwen-code"
	Antigravity    = "antigravity"
	OpenCode       = "opencode"
	CherryStudio   = "cherry-studio"
	Cline          = "cline"
	RooCode        = "roo-code"
	KiloCode       = "kilo-code"
	OpenAISDK      = "openai-sdk"
	AnthropicSDK   = "anthropic-sdk"
	Browser        = "browser"
)

// Identify names the caller of a request from its headers.
func Identify(h http.Header) Client {
	ua := cleanUserAgent(h.Get("User-Agent"))
	name, version := identify(ua, h)
	if name == "" {
		// A version means nothing without the client it belongs to.
		version = ""
	}
	return Client{Name: name, Version: version, UserAgent: ua}
}

func identify(ua string, h http.Header) (string, string) {
	product, version, rest := firstProduct(ua)
	// Codex puts its originator first in the User-Agent ("Codex Desktop/0.169.0
	// (...)", "codex-tui/0.169.0 (...)"); the host app at the end, such as
	// Cursor for the IDE extension, is the editor it runs in, not the client.
	if name, ok := codexName(product); ok {
		return name, version
	}
	switch strings.ToLower(product) {
	case "claude-cli":
		return claudeName(rest), version
	case "geminicli":
		return GeminiCLI, version
	case "qwencode":
		return QwenCode, version
	case "opencode":
		return OpenCode, version
	}
	// These two put their token anywhere in the header: Antigravity after a
	// "vscode/..." prefix, Cherry Studio inside an Electron browser string.
	if v, ok := tokenVersion(ua, "antigravity/"); ok {
		return Antigravity, v
	}
	if v, ok := tokenVersion(ua, "cherrystudio/"); ok {
		return CherryStudio, v
	}
	// VS Code agents call through the OpenAI SDK, whose User-Agent names the SDK;
	// they name themselves in the OpenRouter attribution headers instead.
	if name := extensionName(h); name != "" {
		return name, ""
	}
	switch strings.ToLower(product) {
	case "openai":
		return OpenAISDK, sdkVersion(version, rest)
	case "anthropic":
		return AnthropicSDK, sdkVersion(version, rest)
	}
	if ua == "" {
		// A proxy in between may drop the User-Agent; Codex also sends its
		// originator as a header of its own.
		if name, ok := codexName(h.Get("Originator")); ok {
			return name, ""
		}
		return "", ""
	}
	if strings.EqualFold(product, "mozilla") {
		return Browser, ""
	}
	return tokenName(product), version
}

func codexName(product string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(product)) {
	case "codex desktop", "codex_desktop", "codex-desktop", "codex_app", "codex_chatgpt_desktop", "codex_work_desktop":
		return CodexDesktop, true
	case "codex-tui", "codex_tui", "codex_cli_rs", "codex-cli", "codex_cli":
		return CodexCLI, true
	case "codex_exec", "codex-exec":
		return CodexExec, true
	case "codex_vscode", "codex-vscode":
		return CodexIDE, true
	case "codex_sdk_ts", "codex-sdk":
		return CodexSDK, true
	}
	return "", false
}

// claudeName tells Claude Code from the Agent SDK, which sends the same
// "claude-cli/<version>" product with an "sdk-ts" or "sdk-py" entrypoint:
// "claude-cli/2.0.14 (external, sdk-ts)".
func claudeName(rest string) string {
	open, closing := strings.IndexByte(rest, '('), strings.IndexByte(rest, ')')
	if open >= 0 && closing > open {
		for _, part := range strings.Split(rest[open+1:closing], ",") {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "sdk") {
				return ClaudeAgentSDK
			}
		}
	}
	return ClaudeCode
}

func extensionName(h http.Header) string {
	title := strings.ToLower(strings.TrimSpace(h.Get("X-Title")))
	referer := strings.ToLower(strings.TrimSpace(h.Get("HTTP-Referer")))
	switch {
	case title == "cline" || strings.Contains(referer, "cline.bot"):
		return Cline
	case title == "roo code" || strings.Contains(referer, "roocode.com") || strings.Contains(referer, "roovetgit"):
		return RooCode
	case title == "kilo code" || strings.Contains(referer, "kilocode.ai"):
		return KiloCode
	}
	return ""
}

// sdkVersion joins the language and version of the official SDKs, which send
// "OpenAI/Python 1.109.1" and "Anthropic/JS 0.65.0".
func sdkVersion(lang, rest string) string {
	fields := strings.Fields(rest)
	if lang == "" || len(fields) == 0 || fields[0][0] < '0' || fields[0][0] > '9' {
		return lang
	}
	return cleanVersion(lang + " " + fields[0])
}

// tokenVersion finds a case-insensitive "<name>/<version>" token anywhere in ua.
func tokenVersion(ua, token string) (string, bool) {
	i := strings.Index(strings.ToLower(ua), token)
	if i < 0 {
		return "", false
	}
	v := ua[i+len(token):]
	if end := strings.IndexAny(v, " ;)("); end >= 0 {
		v = v[:end]
	}
	return cleanVersion(v), true
}

// firstProduct splits ua into its first product, that product's version and
// the rest: "Codex Desktop/0.169.0 (Mac OS ...)" gives "Codex Desktop",
// "0.169.0" and " (Mac OS ...)". A header without a slash in its first token
// ("node") is all product.
func firstProduct(ua string) (product, version, rest string) {
	if slash := strings.IndexByte(ua, '/'); slash > 0 && slash <= maxNameLen && !strings.ContainsAny(ua[:slash], "();") {
		product, rest = ua[:slash], ua[slash+1:]
		end := strings.IndexAny(rest, " ;)(")
		if end < 0 {
			end = len(rest)
		}
		return strings.TrimSpace(product), cleanVersion(rest[:end]), rest[end:]
	}
	end := strings.IndexAny(ua, " ;(/")
	if end < 0 {
		end = len(ua)
	}
	return ua[:end], "", ua[end:]
}

// tokenName turns a product token into a name: lower case, spaces and
// underscores as hyphens, only letters, digits, dots and hyphens kept, so the
// value is safe to show, filter on and send back as a query parameter.
func tokenName(product string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(product) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= maxNameLen {
			break
		}
	}
	return strings.Trim(b.String(), "-.")
}

func cleanVersion(v string) string {
	v = strings.TrimSpace(v)
	var b strings.Builder
	for _, r := range v {
		if r <= 0x20 || r >= 0x7f || r == '"' {
			if r == ' ' && b.Len() > 0 {
				b.WriteByte(' ')
			}
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxVersionLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// cleanUserAgent replaces control characters, which have no place in a header
// and would break a log line, and cuts the header to MaxUserAgentBytes without
// splitting a character.
func cleanUserAgent(ua string) string {
	ua = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, ua)
	ua = strings.TrimSpace(ua)
	if len(ua) <= MaxUserAgentBytes {
		return ua
	}
	cut := MaxUserAgentBytes
	for cut > 0 && !utf8.RuneStart(ua[cut]) {
		cut--
	}
	return strings.TrimSpace(ua[:cut])
}
