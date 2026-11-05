package privacymask

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
	// built in two pieces so secret scanners do not take the test value for a key
	testStripeKey  = "sk_" + "test_" + "Zq8vT2xLk9RbW4nZpE6s"
	testPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ\n-----END OPENSSH PRIVATE KEY-----"
)

// foundIn is what masking text with o replaces, as kind:value.
func foundIn(text string, o Options) []string {
	m := NewMasker(testKey, o, nil, "")
	var out []string
	for _, e := range m.Plan([]string{text}, nil)[0] {
		out = append(out, kindOfPlaceholder(e.Text)+":"+text[e.Start:e.End])
	}
	return out
}

var secretsOnly = Options{Secrets: true}

func TestRulesFindWhatIsClearlyASecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"anthropic key", "use " + testAnthropicKey + " for now", []string{"API_KEY:" + testAnthropicKey}},
		{"openai key in an env file", "OPENAI_API_KEY=" + testOpenAIKey + "\nDEBUG=1", []string{"API_KEY:" + testOpenAIKey}},
		{"aws access key", "aws_access_key_id = " + testAWSKey, []string{"API_KEY:" + testAWSKey}},
		{"github token in a url", "https://" + testGitHubToken + "@github.com/o/r.git", []string{"API_KEY:" + testGitHubToken}},
		{"jwt", "the session is " + testJWT, []string{"TOKEN:" + testJWT}},
		{"bearer in a command", "curl -H 'Authorization: Bearer 9f8e7d6c5b4a39281706f5e4d3c2b1a0' https://api.internal", []string{"TOKEN:9f8e7d6c5b4a39281706f5e4d3c2b1a0"}},
		{"connection string", "DATABASE_URL=postgres://app:Tr0ub4dor3@db.internal:5432/app", []string{"PASSWORD:Tr0ub4dor3"}},
		{"password assignment", `DB_PASSWORD="S3cr3t!x9"`, []string{"SECRET:S3cr3t!x9"}},
		{"json secret", `{"client_secret": "a1b2c3d4e5f6g7h8"}`, []string{"SECRET:a1b2c3d4e5f6g7h8"}},
		{"yaml token", "auth_token: Q7wE9rT1yU3iO5pA", []string{"SECRET:Q7wE9rT1yU3iO5pA"}},
		{"chinese password", "服务器密码：Abc12345，别外传", []string{"PASSWORD:Abc12345"}},
		{"chinese key", "私钥是 x9Y8z7W6v5", []string{"SECRET:x9Y8z7W6v5"}},
		{"private key block", "cat ~/.ssh/id_ed25519\n" + testPrivateKey + "\n", []string{"PRIVATE_KEY:" + testPrivateKey}},
		{"password in a sentence", "my password is Tr0ub4dor&3xq, keep it safe", []string{"PASSWORD:Tr0ub4dor&3xq"}},
		{"key in a sentence", "the API key was ak9X2mQ7pL4vR8tZ, rotate it", []string{"SECRET:ak9X2mQ7pL4vR8tZ"}},
		{"stripe test key", "STRIPE=" + testStripeKey, []string{"API_KEY:" + testStripeKey}},
		{"key as a value", "password=" + testOpenAIKey, []string{"API_KEY:" + testOpenAIKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := foundIn(tc.text, secretsOnly); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("found %q, want %q", got, tc.want)
			}
		})
	}
}

// The cases other masking gateways got wrong, and the everyday code and prose
// that look like a secret's name with something else as its value. A false
// positive hides a value the model needs, so each of these must stay as it is.
func TestRulesLeaveOrdinaryTextAlone(t *testing.T) {
	for _, text := range []string{
		"items.sort(key=lambda x: x[1]); sort_key = 'created_at'",
		"sort_key=priority_level_2",
		`{"model": "claude", "max_tokens": 4096, "stream": true}`,
		"max_tokens: 128000",
		"see https://www.sk-hynix.com/products/sk-hynix-hbm3e-memory-module-specifications for the HBM figures",
		"sequential_cutoff = 0.85; sequential_cutoff_ratio=1.5e-3",
		"token = getToken()",
		"api_key = os.environ['OPENAI_API_KEY']",
		"apiKey: process.env.OPENAI_API_KEY,",
		"password: ********",
		"API_KEY=${OPENAI_API_KEY}",
		"access_token: <paste-your-token>",
		"refresh_token: string;",
		"the risk-assessment sk-12 task",
		"tasks_completed: 12, desk-ant-farm",
		"Bearer of bad news",
		"密码长度至少为 8 位",
		"PWD=/home/dev/project",
		// sentences that name a password or a token without one
		"the password is required",
		"your password is too short",
		"the token is expired, sign in again",
		"the secret is out",
		"password was changed by an administrator",
	} {
		if got := foundIn(text, secretsOnly); len(got) > 0 {
			t.Errorf("%q: found %q", text, got)
		}
	}
}

func TestPersonalDataOnlyWhenAskedFor(t *testing.T) {
	text := "call 13800138000 or mail li.wei@acme-corp.cn, card 4111 1111 1111 1111, id 11010519491231002X"
	if got := foundIn(text, secretsOnly); len(got) > 0 {
		t.Fatalf("secrets only, found %q", got)
	}
	got := strings.Join(foundIn(text, Options{Secrets: true, Personal: true}), "|")
	for _, want := range []string{"PHONE:13800138000", "EMAIL:li.wei@acme-corp.cn", "BANK_CARD:4111 1111 1111 1111", "ID_CARD:11010519491231002X"} {
		if !strings.Contains(got, want) {
			t.Errorf("personal data on, %q missing from %q", want, got)
		}
	}
	// a spaced mobile number, as a message writes one; an order number stays
	if got := foundIn("我的手机 139 1234 5678，订单号 13912345678", Options{Personal: true}); strings.Join(got, "|") != "PHONE:139 1234 5678" {
		t.Fatalf("found %q", got)
	}
}

func TestWordsAndCustomRules(t *testing.T) {
	o := Options{Words: []string{"Nightjar", "1990-05-12"}, WordsAnyCase: true, WordsWhole: true}
	got := foundIn("Deploy NIGHTJAR, not Nightjars; born 1990年5月12日", o)
	if strings.Join(got, "|") != "TERM:NIGHTJAR|TERM:1990年5月12日" {
		t.Fatalf("words found %q", got)
	}
	rules, err := CheckRules([]Rule{{Kind: "gateway key", Prefix: "gw_live_"}})
	if err != nil {
		t.Fatal(err)
	}
	got = foundIn("export GW=gw_live_9Hq2mX7kL1zR8wV3", Options{Secrets: true, Rules: rules})
	if strings.Join(got, "|") != "GATEWAY_KEY:gw_live_9Hq2mX7kL1zR8wV3" {
		t.Fatalf("custom rule found %q", got)
	}
	if _, err := CheckRules([]Rule{{Kind: "x", Regex: "a*"}}); err == nil {
		t.Fatal("a pattern that matches the empty string was accepted")
	}
}

func TestAllowListAndActions(t *testing.T) {
	text := "keys: " + testOpenAIKey + " and " + testGitHubToken
	m := NewMasker(testKey, Options{Secrets: true, Allow: []string{testGitHubToken}}, nil, "")
	if out := Apply(text, m.Plan([]string{text}, nil)[0]); strings.Contains(out, testOpenAIKey) || !strings.Contains(out, testGitHubToken) {
		t.Fatalf("allow list: %s", out)
	}
	m = NewMasker(testKey, Options{Secrets: true, Actions: map[string]string{"API_KEY": Warn}}, nil, "")
	edits := m.Plan([]string{text}, nil)[0]
	if len(edits) != 0 || len(m.Values()) != 2 || m.Values()[0].Action != Warn || m.Values()[1].Action != Warn {
		t.Fatalf("warn: edits %v, values %+v", edits, m.Values())
	}
	m = NewMasker(testKey, Options{Secrets: true, Actions: map[string]string{"API_KEY": Block}}, nil, "")
	m.Plan([]string{text}, nil)
	if b := m.Blocked(); len(b) != 2 || b[0].Head != "sk-" || b[0].Len != len(testOpenAIKey) || b[1].Head != "ghp_" {
		t.Fatalf("block: %+v", b)
	}
}
