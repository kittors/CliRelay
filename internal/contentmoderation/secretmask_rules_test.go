package contentmoderation

import (
	"strings"
	"testing"
)

// Fake credentials of the right shapes. None of them was ever issued.
const (
	testAnthropicKey = "sk-ant-api03-Zx8Qm2Lp9Vn4Rt6Yb1Kc3Hd5Jf7Gs0Aw2Ee4Ii6Oo8Uu"
	testOpenAIKey    = "sk-proj-Ab3Cd5Ef7Gh9Ij1Kl3Mn5Op7Qr9St1Uv3Wx5Yz7Ab9Cd"
	testAWSKey       = "AKIAQ3EXAMPLE7Z4K2P9"
	testGitHubToken  = "ghp_4fT9bQ2mX7kL1zR8wV3nJ6cY0hD5sA2eG9uP"
	testJWT          = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
)

func secretTexts(text string) []string {
	var out []string
	for _, span := range findSecrets(text) {
		out = append(out, string(span.kind)+":"+text[span.start:span.end])
	}
	return out
}

func TestFindSecretsMatchesWhatIsClearlyASecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"anthropic key", "use " + testAnthropicKey + " for now", []string{"API_KEY:" + testAnthropicKey}},
		{"openai key in an env file", "OPENAI_API_KEY=" + testOpenAIKey + "\nDEBUG=1", []string{"API_KEY:" + testOpenAIKey}},
		{"aws access key", "aws_access_key_id = " + testAWSKey, []string{"API_KEY:" + testAWSKey}},
		{"github token in a url", "https://" + testGitHubToken + "@github.com/o/r.git", []string{"API_KEY:" + testGitHubToken}},
		{"jwt", "the session is " + testJWT, []string{"JWT:" + testJWT}},
		{"bearer header", "Authorization: Bearer 9f8e7d6c5b4a39281706f5e4d3c2b1a0", []string{"TOKEN:9f8e7d6c5b4a39281706f5e4d3c2b1a0"}},
		{"connection string", "DATABASE_URL=postgres://app:Tr0ub4dor3@db.internal:5432/app", []string{"PASSWORD:Tr0ub4dor3"}},
		{"password assignment", `DB_PASSWORD="S3cr3t!x9"`, []string{"PASSWORD:S3cr3t!x9"}},
		{"json secret", `{"client_secret": "a1b2c3d4e5f6g7h8"}`, []string{"SECRET:a1b2c3d4e5f6g7h8"}},
		{"yaml token", "auth_token: Q7wE9rT1yU3iO5pA", []string{"SECRET:Q7wE9rT1yU3iO5pA"}},
		{"chinese password", "服务器密码：Abc12345，别外传", []string{"PASSWORD:Abc12345"}},
		{"chinese key", "私钥是 x9Y8z7W6v5", []string{"SECRET:x9Y8z7W6v5"}},
		{
			// The prefix rule names it an API key, though it is also a password's value.
			"key as a value", "password=" + testOpenAIKey, []string{"API_KEY:" + testOpenAIKey},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := secretTexts(tc.text); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("found %q, want %q", got, tc.want)
			}
		})
	}
}

// The cases other masking gateways got wrong, and the everyday code that looks
// like a secret's name with something else as its value. A false positive hides
// a value the model needs, so each of these must stay as it is.
func TestFindSecretsLeavesOrdinaryTextAlone(t *testing.T) {
	for _, text := range []string{
		// magpie masked these.
		"items.sort(key=lambda x: x[1]); sort_key = 'created_at'",
		"sort_key=priority_level_2",
		`{"model": "claude", "max_tokens": 4096, "stream": true}`,
		"max_tokens: 128000",
		"see https://www.sk-hynix.com/products/sk-hynix-hbm3e-memory-module-specifications for the HBM figures",
		"sequential_cutoff = 0.85; sequential_cutoff_ratio=1.5e-3",
		// Names of secrets with no secret in them.
		"token = getToken()",
		"api_key = os.environ['OPENAI_API_KEY']",
		"apiKey: process.env.OPENAI_API_KEY,",
		"secret_key = settings.SECRET_KEY",
		"password: ********",
		"api_key: your_api_key_here",
		"token: required",
		`password: "password"`,
		"API_KEY=${OPENAI_API_KEY}",
		"token: {{ .Values.token }}",
		"access_token: <paste-your-token>",
		"password = request.form['password']",
		"next_token = logits.argmax()",
		"refresh_token: string;",
		// Prefixes inside longer words, and too short to be keys.
		"the risk-assessment sk-12 task",
		"tasks_completed: 12, desk-ant-farm",
		"Bearer of bad news",
		"https://example.com/docs#token=abc",
		"密码长度至少为 8 位",
	} {
		if got := secretTexts(text); len(got) > 0 {
			t.Errorf("%q: found %q", text, got)
		}
	}
}

func TestFindSecretsReturnsSpansInOrderWithoutOverlap(t *testing.T) {
	text := "a=" + testJWT + " b: Bearer " + testOpenAIKey + " c " + testAWSKey
	spans := findSecrets(text)
	if len(spans) != 3 {
		t.Fatalf("spans = %+v", spans)
	}
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			t.Fatalf("spans overlap: %+v", spans)
		}
	}
	if got := text[spans[1].start:spans[1].end]; got != testOpenAIKey || spans[1].kind != secretAPIKey {
		t.Fatalf("the bearer token was matched as %s:%q, want the API key", spans[1].kind, got)
	}
}

func TestMaskedValuesNeverNeedEscaping(t *testing.T) {
	for _, text := range []string{
		`password = "abc\"def12345"`,
		`token: abcAdef12345`,
		"secret: héllo12345678",
	} {
		for _, span := range findSecrets(text) {
			if value := text[span.start:span.end]; !isSecretValue(value) || strings.ContainsAny(value, "\"\\{}") {
				t.Errorf("%q: masked %q, which JSON would escape or a placeholder could not stand for", text, value)
			}
		}
	}
}
