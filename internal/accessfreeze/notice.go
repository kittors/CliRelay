package accessfreeze

import (
	"strings"
	"time"
)

// providerNames are the vendor names a requester reads for the providers the
// gateway ships executors for. A provider that is not listed is an
// operator-named OpenAI-compatible upstream: its name is the channel's, which a
// portal user must not learn, so the notice calls it "this upstream" instead.
var providerNames = map[string]string{
	"aistudio":     "AI Studio",
	"antigravity":  "Antigravity",
	"bedrock":      "Bedrock",
	"claude":       "Claude",
	"cline":        "Cline",
	"codex":        "Codex",
	"commandcode":  "Command Code",
	"gemini":       "Gemini",
	"gemini-cli":   "Gemini CLI",
	"iflow":        "iFlow",
	"kimi":         "Kimi",
	"minimax":      "MiniMax",
	"ollama-cloud": "Ollama Cloud",
	"opencode-go":  "OpenCode Go",
	"qwen":         "Qwen",
	"vertex":       "Vertex AI",
	"xai":          "xAI",
}

// ProviderDisplayName is the name a requester may read for provider, and
// whether the provider is one the gateway knows by name.
func ProviderDisplayName(provider string) (string, bool) {
	name, ok := providerNames[normalizeProvider(provider)]
	return name, ok
}

// refusalReasons are the requester-facing reasons for the refusal codes the
// gateway acts on (upstreamContentRefusalCodes in sdk/cliproxy/auth), in
// English and Chinese.
var refusalReasons = map[string][2]string{
	"cyber_policy": {"possible cybersecurity risk", "可能存在网络安全风险"},
}

// noticeTimeLayout names the offset: requesters read the notice in clients
// anywhere, and an abbreviation such as CST is ambiguous.
const noticeTimeLayout = "2006-01-02 15:04 -07:00"

// Notice is what the requester reads when a request is refused for the
// freeze: what is frozen, since when, why, that other providers still work,
// and who can lift it. It is in English for clients and tooling, then in
// Chinese for the portal's users.
func Notice(f Freeze, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	at := f.FrozenAt.In(loc).Format(noticeTimeLayout)
	nameEN, nameZH := "this upstream", "该上游"
	if name, ok := ProviderDisplayName(f.Provider); ok {
		nameEN, nameZH = name, name
	}
	holderEN, holderZH := "this account", "本账号"
	if f.Subject.Kind == SubjectAPIKey {
		holderEN, holderZH = "this API key", "该 API Key"
	}
	code := strings.TrimSpace(f.ReasonCode)
	reasonEN, reasonZH := "for its content", "以内容风险为由"
	if reason, ok := refusalReasons[code]; ok {
		reasonEN, reasonZH = "for "+reason[0], "以"+reason[1]+"为由"
	}
	if code != "" {
		reasonEN += " (" + code + ")"
		reasonZH += "（" + code + "）"
	}
	en := "Access to " + nameEN + " is frozen for " + holderEN + " since " + at +
		": the upstream's safety check refused a request from it " + reasonEN +
		". Other upstreams are not affected. Contact your administrator to restore it."
	zh := holderZH + "的 " + nameZH + " 使用权限已于 " + at + " 冻结：上游安全检查" + reasonZH +
		"拒绝了来自" + holderZH + "的请求。其他上游不受影响，如需恢复请联系管理员。"
	return en + " " + zh
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
