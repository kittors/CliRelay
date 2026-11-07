package secretscrub

import (
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// nest wraps doc in depth JSON documents, each holding the previous one as a
// string, the way request details hold an upstream request body.
func nest(doc string, depth int) string {
	for i := 0; i < depth; i++ {
		data, err := json.Marshal(map[string]string{"log": doc})
		if err != nil {
			panic(err)
		}
		doc = string(data)
	}
	return doc
}

// unnest undoes nest, checking that every level is still valid JSON.
func unnest(t *testing.T, doc string, depth int) string {
	t.Helper()
	for i := 0; i < depth; i++ {
		if !json.Valid([]byte(doc)) {
			t.Fatalf("level %d is not valid JSON: %q", i, doc)
		}
		var wrapper map[string]string
		if err := json.Unmarshal([]byte(doc), &wrapper); err != nil {
			t.Fatalf("level %d: %v", i, err)
		}
		doc = wrapper["log"]
	}
	return doc
}

var credentialFragments = []string{
	openAIProjectKey, anthropicKey, relayClientKey, googleAPIKey, githubToken, slackToken,
	awsAccessKeyID, sampleJWT, panelSession,
	"DB_PASSWORD=hunter2", "API_TOKEN=abcd1234efgh5678", `"client_secret": "GOCSPX-abc123def456"`,
	"postgres://app:s3cr3t-Pa55@db:5432/app", "Authorization: Bearer 0a1b2c3d4e5f6a7b8c9d",
	"Cookie: session=9f8e7d6c; csrftoken=abc", "Basic dXNlcjpwYXNzd29yZA==", "密码：Abc12345",
	"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----",
	"-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBkg4LVWM9nuwNSk3yByxZpYRTBnVJk5oG",
}

var fillerFragments = []string{
	" ", "\n", "\t", `"`, `'`, `\`, `\\`, `\"`, "{", "}", "[", "]", ":", "=", ",", ";", "&",
	"password", "token", "key", "secret", "Bearer ", "Basic ", "://", "@", "-----BEGIN ", "eyJ",
	"sk-", "max_tokens", "naïve", "密码", "令牌", "é", " ", "\x00", "\x7f",
}

// Scrubbing never breaks JSON, however deep the credential sits in nested
// documents, and never leaves a credential behind at the innermost level.
func TestScrubKeepsNestedJSONValid(t *testing.T) {
	for depth := 1; depth <= 4; depth++ {
		for _, fragment := range credentialFragments {
			doc := nest("before "+fragment+" after", depth)
			got := Scrub(doc)
			inner := unnest(t, got, depth)
			if inner == "before "+fragment+" after" {
				t.Errorf("depth %d: nothing replaced in %q", depth, fragment)
			}
		}
	}
}

func TestScrubKeepsRandomJSONValid(t *testing.T) {
	random := rand.New(rand.NewSource(20261030))
	for i := 0; i < 3000; i++ {
		var text strings.Builder
		for j := random.Intn(12); j >= 0; j-- {
			if random.Intn(3) == 0 {
				text.WriteString(credentialFragments[random.Intn(len(credentialFragments))])
			} else {
				text.WriteString(fillerFragments[random.Intn(len(fillerFragments))])
			}
		}
		depth := 1 + random.Intn(3)
		doc := nest(text.String(), depth)
		got := Scrub(doc)
		unnest(t, got, depth)
		if again := Scrub(got); again != got {
			t.Fatalf("not idempotent on %q", doc)
		}
	}
}

func FuzzScrubKeepsJSONValid(f *testing.F) {
	for _, fragment := range credentialFragments {
		f.Add(fragment)
	}
	for _, fragment := range fillerFragments {
		f.Add(fragment)
	}
	f.Fuzz(func(t *testing.T, text string) {
		for depth := 1; depth <= 2; depth++ {
			doc := nest(text, depth)
			unnest(t, Scrub(doc), depth)
		}
	})
}

// A valid JSON document stays valid, whatever it holds: credentials named in
// keys, in string values, in arrays next to numbers, in nested documents.
func FuzzScrubKeepsDocumentsValid(f *testing.F) {
	for _, seed := range []string{
		`{"log":"Cookie:"}`,
		`["token:", 12345678, " "]`,
		`{"Cookie:": "session=abc123"}`,
		`{"password": 123456, "secret": null, "api_key": true, "token": ["a1b2c3d4e5"]}`,
		`{"password": "hunter22", "headers": {"Authorization": ["Bearer 0a1b2c3d4e5f6a7b8c9d"]}}`,
		`{"a":"password:","b":"x"}`,
		`{"text":"DB_PASSWORD=hunter2\nAPI_TOKEN=\"abcd1234efgh5678\"","n":1}`,
		`{"inner":"{\"secret_key\":\"q9w8e7r6t5y4\",\"list\":[\"token:\", 1, \" \"]}"}`,
		`[{"type":"text","text":"密码：Abc12345"},{"type":"text","text":"postgres://u:p4ssw0rd@h/db"}]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		if !json.Valid([]byte(doc)) {
			return
		}
		if got := Scrub(doc); !json.Valid([]byte(got)) {
			t.Fatalf("broke a valid document:\n in: %q\nout: %q", doc, got)
		}
	})
}

// agentRequest builds a request body like a coding agent's: a long system
// prompt, tool schemas, code and file contents, about size bytes of it.
func agentRequest(size int) string {
	turn := map[string]any{
		"role": "user",
		"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_01", "content": "package auth\n\n// Login checks the password and returns a session token.\nfunc Login(user, password string) (token string, err error) {\n\tkey := sortKey(user)\n\tif maxTokens := cfg.MaxTokens; maxTokens > 0 {\n\t\treturn issueToken(key, maxTokens)\n\t}\n\treturn \"\", errInvalidKey\n}\n"},
			{"type": "text", "text": "Use the API key from the config, keep max_tokens at 4096, and make sure the token cache key is stable. 请检查令牌桶的密钥轮换逻辑。"},
		},
	}
	system := strings.Repeat("You are a coding agent. Never print secrets such as an API key, a password or an access token; refer to them by name. Use the Bash tool to run commands and the Read tool to read files. ", 20)
	tools := []map[string]any{{
		"name":         "Bash",
		"description":  "Runs a shell command. The token budget for output is limited.",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "number"}}},
	}}
	var messages []any
	request := map[string]any{"model": "claude-opus-5-5", "max_tokens": 32000, "system": system, "tools": tools}
	for {
		messages = append(messages, turn)
		request["messages"] = messages
		data, _ := json.Marshal(request)
		if len(data) >= size {
			return string(data)
		}
	}
}

func BenchmarkScrubAgentRequest(b *testing.B) {
	request := agentRequest(1 << 20)
	b.SetBytes(int64(len(request)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scrub(request)
	}
}

// imageRequest is a request carrying an inline image of size random bytes,
// base64-encoded the way clients send screenshots.
func imageRequest(size int) string {
	image := make([]byte, size)
	rand.New(rand.NewSource(20261030)).Read(image)
	data, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "What does this screenshot show?"},
		{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)}},
	}}}})
	return string(data)
}

func BenchmarkScrubInlineImage(b *testing.B) {
	request := imageRequest(1 << 20)
	b.SetBytes(int64(len(request)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scrub(request)
	}
}

// Random base64 holds every prefix the rules know sooner or later; inside a
// base64 run none of them starts a credential.
func TestInlineImageHasNothingToReplace(t *testing.T) {
	request := imageRequest(1 << 20)
	if got := Scrub(request); got != request {
		t.Fatalf("replaced text in an inline image: %s", firstDifference(request, got))
	}
}

func TestAgentRequestHasNothingToReplace(t *testing.T) {
	request := agentRequest(64 << 10)
	if got := Scrub(request); got != request {
		t.Fatalf("replaced text in an ordinary agent request: %s", firstDifference(request, got))
	}
}

func firstDifference(a, b string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			start := max(0, i-60)
			return "…" + b[start:min(len(b), i+80)] + "…"
		}
	}
	return "lengths differ"
}
