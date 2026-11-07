package secretscrub

import (
	"strings"
	"testing"
)

// Credentials below are made up; each has the shape the rules look for. The
// sk- ones carry an X, the repository's mark of a fake key for the secret scan
// in scripts/ci-pr.sh, and none carries the checksums or markers that GitHub's
// push protection takes for a live key (OpenAI's T3BlbkFJ, Slack's numeric
// team and bot ids).
const (
	openAIProjectKey = "sk-proj-Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4zAb7dEf0hIj3kLm6nOp9qRs2t"
	anthropicKey     = "sk-ant-api03-Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2Ji1Hg0FeDcBa9Zx8Yw7Vu6Ts5Rq4Po3Nm2Lk1Ji0Hg-AbCdEfGh"
	legacyOpenAIKey  = "sk-Q8xLm2NX7Pz4Rw9Kv3Ty6Hs1Jd5Gf8Ac2BeR4tY"
	relayClientKey   = "sk-k3j9x2mq7v8b1n4c6z0p5r2t8w3y6u9X"
	googleAPIKey     = "AIzaSyD4x9Kq2Lm7Np3Rv8Tz1Wb6Yc5Fh0Jg2Ks"
	githubToken      = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	slackToken       = "xoxb-NotARealTeam-NotARealBot-AbCdEfGhIjKlMnOpQrStUvWx"
	awsAccessKeyID   = "AKIAQ3X7V9M2KZ4T8WPL"
	sampleJWT        = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	panelSession     = "cps_q8Zk3Lm9Np2Rv7Tz1Wb6Yc5Fh0Jg4Ks8Dx3Mv7Pq2"
)

func TestScrubRecognisesCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		secret string // must be gone after scrubbing
		kind   string
	}{
		{"openai project key", "OPENAI_KEY=" + openAIProjectKey, openAIProjectKey, "openai_key"},
		{"anthropic key", `{"x-api-key":"` + anthropicKey + `"}`, anthropicKey, "anthropic_key"},
		{"legacy openai key in prose", "use " + legacyOpenAIKey + " for now", legacyOpenAIKey, "api_key"},
		{"relay client key", "curl -H 'x-api-key: " + relayClientKey + "'", relayClientKey, "api_key"},
		{"key after a JSON newline escape", `"text":"line one\n` + relayClientKey + `\nline three"`, relayClientKey, "api_key"},
		{"key after a nested newline escape", `"text":"one\\\\n` + relayClientKey + `"`, relayClientKey, "api_key"},
		{"google api key", "https://maps.googleapis.com/maps/api/js?key=" + googleAPIKey + "&v=3", googleAPIKey, "google_api_key"},
		{"github token", "git remote set-url origin https://" + githubToken + "@github.com/o/r.git", githubToken, "github_token"},
		{"slack token", "SLACK_BOT=" + slackToken, slackToken, "slack_token"},
		{"aws access key id", "aws_access_key_id = " + awsAccessKeyID, awsAccessKeyID, "aws_access_key_id"},
		{"jwt", `{"id_token":"` + sampleJWT + `"}`, sampleJWT, "jwt"},
		{"panel session", `{"session":"` + panelSession + `"}`, panelSession, "clirelay_token"},
		{"database url", "DATABASE_URL=postgres://app:s3cr3t-Pa55@db.internal:5432/app", "s3cr3t-Pa55", "url_password"},
		{"redis url without user", "redis://:hunter22@10.0.0.5:6379/0", "hunter22", "url_password"},
		{"env password", "DB_HOST=db\nDB_PASSWORD=hunter2\nDB_PORT=5432", "hunter2", "password"},
		{"escaped env password", `"content":"DB_HOST=db\nDB_PASSWORD=hunter2\nDB_PORT=5432"`, "hunter2", "password"},
		{"env token", "export API_TOKEN=abcd1234efgh5678", "abcd1234efgh5678", "secret"},
		{"yaml client secret", "client_secret: 'GOCSPX-abc123def456ghi'", "GOCSPX-abc123def456ghi", "secret"},
		{"json password with spaces", `{"password": "correct horse battery staple"}`, "correct horse battery staple", "password"},
		{"escaped json field", `"arguments":"{\"api_key\":\"abc123def456ghi789\"}"`, "abc123def456ghi789", "secret"},
		{"double escaped json field", `"log":"{\\\"secret_key\\\":\\\"q9w8e7r6t5y4\\\"}"`, "q9w8e7r6t5y4", "secret"},
		{"camel case field", `{"SecretAccessKey":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYzQ8PL2KSVT"}`, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYzQ8PL2KSVT", "secret"},
		{"go assignment", `password := "Tr0ub4dor&3"`, "Tr0ub4dor&3", "password"},
		{"cli flag", "mysql -u root --password=Qwerty123! db", "Qwerty123!", "password"},
		{"query string", "/login?user=bob&password=pa55w0rd&next=/", "pa55w0rd", "password"},
		{"letters only password", "SMTP_PASSWORD=supersecret", "supersecret", "password"},
		{"authorization header in a dump", "Headers:\nAuthorization: Bearer sk-p...wxyz\nContent-Type: application/json", "sk-p...wxyz", "authorization"},
		{"cookie header", "Cookie: session=9f8e7d6c; csrftoken=abc\nAccept: */*", "session=9f8e7d6c; csrftoken=abc", "cookie"},
		{"bearer in curl", `curl -H "Authorization: Bearer 0a1b2c3d4e5f6a7b8c9d"`, "0a1b2c3d4e5f6a7b8c9d", "authorization"},
		{"bearer in prose", "send it as bearer 8f2a9c3e1b7d4f6a0e5c please", "8f2a9c3e1b7d4f6a0e5c", "bearer_token"},
		{"basic credential", "curl -H 'X: Basic dXNlcjpwYXNzd29yZA=='", "dXNlcjpwYXNzd29yZA==", "basic_auth"},
		{"chinese password", "数据库密码：Abc12345，请妥善保管", "Abc12345", "password"},
		{"chinese password is", "我的密码是 hunter22。", "hunter22", "password"},
		{"chinese token", "访问令牌为tok_8f3a9c2e", "tok_8f3a9c2e", "secret"},
		{"japanese password", "パスワードは pass1234 です", "pass1234", "password"},
		{"korean password", "비밀번호: qwer1234!", "qwer1234!", "password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.text)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("secret survived: %q", got)
			}
			if !strings.Contains(got, "[REDACTED:"+tc.kind+"]") {
				t.Fatalf("want [REDACTED:%s] in %q", tc.kind, got)
			}
		})
	}
}

// The Authorization scheme stays: which one a client used helps
// troubleshooting and says nothing about the credential.
func TestScrubKeepsTheAuthorizationScheme(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"Authorization: Bearer sk-p...wxyz\n", "Authorization: Bearer [REDACTED:authorization]\n"},
		{"Authorization: Basic dXNlcjpwYXNzd29yZA==\n", "Authorization: Basic [REDACTED:authorization]\n"},
		{
			"Authorization: AWS4-HMAC-SHA256 Credential=" + awsAccessKeyID + "/20261030/us-east-1/s3/aws4_request, Signature=fe5f80f77d5f",
			"Authorization: AWS4-HMAC-SHA256 [REDACTED:authorization]",
		},
		// Where a recognised format covers the whole credential, its kind is
		// the more specific one.
		{`{"authorization":"Bearer ` + panelSession + `"}`, `{"authorization":"Bearer [REDACTED:clirelay_token]"}`},
		{"Authorization: " + anthropicKey + "\n", "Authorization: [REDACTED:anthropic_key]\n"},
		{"Authorization: Bearer\n", "Authorization: [REDACTED:authorization]\n"},
	} {
		if got := Scrub(tc.text); got != tc.want {
			t.Errorf("Scrub(%q)\n got %q\nwant %q", tc.text, got, tc.want)
		}
	}
}

func TestScrubRemovesPrivateKeys(t *testing.T) {
	body := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\nq3VHg9Xk2m4nL8pR5tW1yZ0aB6cD9eF2gH5iJ8kL1mN4oP7qR0sT"
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----"
	for _, tc := range []struct{ name, text string }{
		{"raw", "key file:\n" + pem + "\nend"},
		{"json escaped", `{"content":"` + strings.ReplaceAll(pem, "\n", `\n`) + `"}`},
		{"double escaped", `{"log":"` + strings.ReplaceAll(pem, "\n", `\\n`) + `"}`},
		{"cut off before its end line", `{"content":"` + strings.ReplaceAll("-----BEGIN OPENSSH PRIVATE KEY-----\n"+body, "\n", `\n`) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Scrub(tc.text)
			if strings.Contains(got, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7") || strings.Contains(got, "q3VHg9Xk2m4nL8pR5tW1yZ0aB6cD9eF2gH5iJ8kL1mN4oP7qR0sT") {
				t.Fatalf("key body survived: %q", got)
			}
			if !strings.Contains(got, "[REDACTED:private_key]") {
				t.Fatalf("want [REDACTED:private_key] in %q", got)
			}
		})
	}
}

// Text that only looks like a credential stays as it is. Several of these are
// the misfires other redactors shipped with: sort_key, max_tokens, an article
// URL about SK hynix, sequential_cutoff.
func TestScrubLeavesOrdinaryTextAlone(t *testing.T) {
	for _, text := range []string{
		`{"model":"m","max_tokens":4096,"usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":0}}`,
		`results.sort(key=lambda r: r.score); sort_key = 3; token_count = 12`,
		`see https://news.example.com/tech/sk-hynix-hbm4-mass-production-2026 for details`,
		`sequential_cutoff = 0.5`,
		`api_key = os.getenv("OPENAI_API_KEY")`,
		`client = OpenAI(api_key=api_key)`,
		`interface Login { password: string; token?: string }`,
		`def login(password: str, token: Optional[str] = None):`,
		`token = getToken()`,
		`password = request.form["password"]`,
		`password = config.password`,
		`secret: false`,
		`{"password": "<your password>", "api_key": "YOUR_API_KEY"}`,
		`PASSWORD=${DB_PASSWORD}`,
		`token: "{{ .Token }}"`,
		`fetch(url, {credentials: "include"})`,
		`The password must be at least 8 characters.`,
		`Bearer tokens are sent in the Authorization header.`,
		`Basic usage: run the tool.`,
		`{"logprobs":{"content":[{"token":"Hello","logprob":-0.31}]}}`,
		`{"eos_token": "</s>", "pad_token": "[PAD]", "bos_token": "<|begin_of_text|>"}`,
		`{"key": "value", "cache_key": "abc123", "prompt_cache_key": "019a1b2c3d4e5f6a"}`,
		`{"encrypted_content":"gAAAAABmZ3x9Kq2Lm7Np3Rv8Tz1Wb6Yc5Fh0Jg2Ks4Dx9Mv7Pq2","signature":"EqQBCkYIBRgCKkB4"}`,
		`x = 1//0`,
		`PWD=/home/user/project`,
		`ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7q3VHg9Xk2m4nL8pR5tW1yZ0aB6cD9eF2gH5i user@host`,
		"-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAu1SU1L\n-----END PUBLIC KEY-----",
		"-----BEGIN CERTIFICATE-----\nMIIDdzCCAl+gAwIBAgIEAgAAuTANBgkqhkiG9w0BAQUFADBaMQsw\n-----END CERTIFICATE-----",
		`密码学是一门学科，令牌桶算法用于限流`,
		`Authorization: <redacted>`,
		`{"client":{"headers":{"Authorization":["\u003credacted\u003e"],"Cookie":["\u003credacted\u003e"]}}}`,
		`{"password": 123456, "secret": null, "api_key": true}`,
		`https://user@example.com/path and git@github.com:o/r.git`,
		`https://user:password@example.com/`,
		`masked: sk-p...wxyz`,
	} {
		if got := Scrub(text); got != text {
			t.Errorf("changed ordinary text:\n in: %q\nout: %q", text, got)
		}
	}
}

// Inline images and files are long base64 runs; a credential prefix that
// turns up inside one by chance is not a credential.
func TestScrubIgnoresPrefixesInsideBase64(t *testing.T) {
	blob := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk", 4)
	text := `{"type":"image","source":{"data":"` + blob + "AIzaSyD4x9Kq2Lm7Np3Rv8Tz1Wb6Yc5Fh0Jg2Ks" + blob +
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fw" + blob + `"}}`
	if got := Scrub(text); got != text {
		t.Fatalf("changed base64 data: %q", got)
	}
}

func TestScrubIsIdempotent(t *testing.T) {
	text := "DB_PASSWORD=hunter2 " + openAIProjectKey + " postgres://u:p4ssw0rd@h/db Authorization: Bearer " + panelSession
	once := Scrub(text)
	if twice := Scrub(once); twice != once {
		t.Fatalf("second pass changed the text:\nonce:  %q\ntwice: %q", once, twice)
	}
}

func TestScrubReturnsCleanTextUnchanged(t *testing.T) {
	text := strings.Repeat("plain text without anything to hide. ", 100)
	if got := Scrub(text); got != text {
		t.Fatal("changed clean text")
	}
	data := []byte(text)
	if got := ScrubBytes(data); &got[0] != &data[0] {
		t.Fatal("ScrubBytes copied clean data")
	}
}
