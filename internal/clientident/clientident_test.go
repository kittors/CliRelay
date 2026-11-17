package clientident

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIdentify(t *testing.T) {
	tests := []struct {
		name        string
		ua          string
		headers     map[string]string
		wantName    string
		wantVersion string
	}{
		// User-Agents seen on the production gateway on 2026-11-17, most frequent first.
		{"codex desktop on windows", "Codex Desktop/0.169.0-alpha.11.1 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.1111.21308)", nil, CodexDesktop, "0.169.0-alpha.11.1"},
		{"codex cli", "codex-tui/0.169.0 (Mac OS 27.0.1; arm64) Apple_Terminal/488 (codex-tui; 0.167.2)", nil, CodexCLI, "0.169.0"},
		{"codex desktop driven from the android app", "Codex Desktop/0.167.0-alpha.4.1 (Mac OS 15.5.0; arm64) unknown (codex_chatgpt_android_remote; dev)", nil, CodexDesktop, "0.167.0-alpha.4.1"},
		{"codex ide extension inside cursor", "codex_vscode/0.146.0-alpha.3 (Windows 10.0.26300; x86_64) unknown (Cursor; 26.721.30844)", nil, CodexIDE, "0.146.0-alpha.3"},
		{"deepseek harness", "deepseek-harness/0.2.2-rc.2 (+https://github.com/deepseek-ai/deepseek-harness) dsh-profile/desktop", nil, "deepseek-harness", "0.2.2-rc.2"},
		{"workbuddy", "WorkBuddy/5.9.2 WorkBuddy/5.9.2 CLI/2.165.0", nil, "workbuddy", "5.9.2"},
		{"balance checker", "KeyBalance/1.0", nil, "keybalance", "1.0"},
		{"curl", "curl/8.7.1", nil, "curl", "8.7.1"},
		{"go client", "Go-http-client/2.0", nil, "go-http-client", "2.0"},
		{"python requests", "python-requests/2.34.2", nil, "python-requests", "2.34.2"},
		{"aiohttp", "Python/3.12 aiohttp/3.14.1", nil, "python", "3.12"},
		{"bare node fetch", "node", nil, "node", ""},
		{"translator app", "Bob/1.21.0 (com.hezongyidev.Bob; build:260; macOS 27.0.1) Alamofire/5.6.2", nil, "bob", "1.21.0"},
		{"browser", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36", nil, Browser, ""},

		// Other Codex originators.
		{"codex cli, older name", "codex_cli_rs/0.98.0 (Ubuntu 24.4.0; x86_64) xterm-256color", nil, CodexCLI, "0.98.0"},
		{"codex exec", "codex_exec/0.150.0 (Mac OS 15.6.0; arm64) unknown", nil, CodexExec, "0.150.0"},
		{"codex sdk", "codex_sdk_ts/0.150.0 (Linux 6.8.0; x86_64) unknown", nil, CodexSDK, "0.150.0"},
		{"an unknown codex originator keeps its own name", "codex_atlas/1.2.0 (Mac OS 15.6.0; arm64)", nil, "codex-atlas", "1.2.0"},
		{"originator without a user agent", "", map[string]string{"Originator": "codex_cli_rs"}, CodexCLI, ""},

		// Clients from their documented User-Agents.
		{"claude code", "claude-cli/2.0.14 (external, cli)", nil, ClaudeCode, "2.0.14"},
		{"claude agent sdk", "claude-cli/2.0.14 (external, sdk-ts)", nil, ClaudeAgentSDK, "2.0.14"},
		{"gemini cli", "GeminiCLI/0.9.0 (darwin; arm64)", nil, GeminiCLI, "0.9.0"},
		{"qwen code", "QwenCode/0.0.14 (darwin; arm64)", nil, QwenCode, "0.0.14"},
		{"opencode", "opencode/0.15.2", nil, OpenCode, "0.15.2"},
		{"antigravity", "antigravity/1.11.3 darwin/arm64", nil, Antigravity, "1.11.3"},
		{"antigravity behind a vscode prefix", "vscode/1.104.0 (antigravity/1.11.3)", nil, Antigravity, "1.11.3"},
		{"cherry studio inside electron", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) CherryStudio/1.6.3 Chrome/138.0.7204.100 Electron/37.2.3 Safari/537.36", nil, CherryStudio, "1.6.3"},
		{"openai python sdk", "OpenAI/Python 1.109.1", nil, OpenAISDK, "Python 1.109.1"},
		{"anthropic js sdk", "Anthropic/JS 0.65.0", nil, AnthropicSDK, "JS 0.65.0"},
		{"cline names itself in x-title", "OpenAI/JS 5.20.0", map[string]string{"X-Title": "Cline", "HTTP-Referer": "https://cline.bot"}, Cline, ""},
		{"roo code", "OpenAI/JS 4.104.0", map[string]string{"X-Title": "Roo Code"}, RooCode, ""},
		{"kilo code from its referer", "OpenAI/JS 4.104.0", map[string]string{"HTTP-Referer": "https://kilocode.ai"}, KiloCode, ""},

		// Nothing to go on.
		{"no headers", "", nil, "", ""},
		{"a product token with no character a name can keep", "客户端/1.0", nil, "", ""},
		{"control characters do not leak into the name", "evil\r\nX-Injected: 1", nil, "evil", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.ua != "" {
				h.Set("User-Agent", tc.ua)
			}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got := Identify(h)
			if got.Name != tc.wantName || got.Version != tc.wantVersion {
				t.Fatalf("Identify(%q) = %q %q, want %q %q", tc.ua, got.Name, got.Version, tc.wantName, tc.wantVersion)
			}
		})
	}
}

func TestIdentifyKeepsTheUserAgentSafeToStore(t *testing.T) {
	t.Run("control characters are replaced", func(t *testing.T) {
		got := Identify(http.Header{"User-Agent": {"a\r\nb\tc\x00d"}})
		if got.UserAgent != "a  b c d" {
			t.Fatalf("UserAgent = %q", got.UserAgent)
		}
	})
	t.Run("a long header is cut without splitting a character", func(t *testing.T) {
		ua := strings.Repeat("a", MaxUserAgentBytes-1) + "界界"
		got := Identify(http.Header{"User-Agent": {ua}})
		if len(got.UserAgent) > MaxUserAgentBytes || !utf8.ValidString(got.UserAgent) {
			t.Fatalf("UserAgent is %d bytes, valid UTF-8 %v", len(got.UserAgent), utf8.ValidString(got.UserAgent))
		}
		if got.UserAgent != strings.Repeat("a", MaxUserAgentBytes-1) {
			t.Fatalf("UserAgent = %q", got.UserAgent)
		}
	})
	t.Run("names and versions are bounded", func(t *testing.T) {
		got := Identify(http.Header{"User-Agent": {strings.Repeat("x", 39) + "/" + strings.Repeat("9", 80)}})
		if len(got.Name) > maxNameLen || len(got.Version) > maxVersionLen {
			t.Fatalf("name %d bytes, version %d bytes", len(got.Name), len(got.Version))
		}
		got = Identify(http.Header{"User-Agent": {strings.Repeat("y", 90)}})
		if len(got.Name) == 0 || len(got.Name) > maxNameLen {
			t.Fatalf("name = %q", got.Name)
		}
	})
}
