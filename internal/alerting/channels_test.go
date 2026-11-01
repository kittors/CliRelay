package alerting

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sampleMessage() Message {
	subject := Subject{Kind: SubjectAccount, ID: "acct-1", Name: "alice@example.com"}
	text := "codex · alice@example.com: weekly window is 92% used (8% left)"
	return Message{
		Version: payloadVersion, ID: "msg-1", Time: timeNow(), TenantID: "tenant-1", Rule: RuleQuotaWindow,
		Severity: SeverityWarning, Language: LanguageEN, Title: "Account quota is almost used up", Text: text, Subject: subject,
		Items: []Item{{Subject: subject, Text: text, Facts: map[string]any{"used_percent": 92.0}}},
	}
}

func sendTo(t *testing.T, rt http.RoundTripper, channel Channel, trusted bool) Delivery {
	t.Helper()
	d := newDispatcherWith(rt)
	d.now = timeNow
	got := d.Send(context.Background(), []Channel{channel}, sampleMessage(), trusted)
	if len(got) != 1 {
		t.Fatalf("Send returned %d results", len(got))
	}
	return got[0]
}

func TestWebhookPayloadAndSignature(t *testing.T) {
	rec := &fakeReceiver{}
	channel := Channel{ID: "c1", Kind: KindWebhook, Name: "ops", URL: testWebhookURL, Secret: testWebhookToken, Enabled: true}
	if got := sendTo(t, rec, channel, false); !got.OK() {
		t.Fatalf("delivery failed: %+v", got)
	}
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	req := reqs[0]
	if req.URL != testWebhookURL {
		t.Errorf("URL = %q", req.URL)
	}
	if got := req.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	if req.Header.Get("X-CliRelay-Event") != RuleQuotaWindow || req.Header.Get("X-CliRelay-Delivery") != "msg-1" {
		t.Errorf("headers = %v", req.Header)
	}

	// The receiver can verify the signature from what it was sent.
	ts := req.Header.Get("X-CliRelay-Timestamp")
	mac := hmac.New(sha256.New, []byte(testWebhookToken))
	mac.Write([]byte(ts + "."))
	mac.Write(req.Body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); req.Header.Get("X-CliRelay-Signature") != want {
		t.Errorf("signature = %q, want %q", req.Header.Get("X-CliRelay-Signature"), want)
	}

	var payload map[string]any
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "id", "time", "tenant_id", "rule", "severity", "language", "title", "text", "subject", "items"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload has no %q: %s", key, req.Body)
		}
	}
	// Nothing that could be a secret travels in the payload.
	for _, secret := range []string{testWebhookToken, testWebhookURL, "hooks.example.com", "secret"} {
		if strings.Contains(string(req.Body), secret) {
			t.Errorf("the payload contains %q: %s", secret, req.Body)
		}
	}
}

func TestWebhookWithoutSecretIsNotSigned(t *testing.T) {
	rec := &fakeReceiver{}
	channel := Channel{ID: "c1", Kind: KindWebhook, Name: "ops", URL: testWebhookURL, Enabled: true}
	if got := sendTo(t, rec, channel, false); !got.OK() {
		t.Fatalf("delivery failed: %+v", got)
	}
	if h := rec.all()[0].Header; h.Get("X-CliRelay-Signature") != "" || h.Get("X-CliRelay-Timestamp") != "" {
		t.Errorf("an unsigned channel sent signature headers: %v", h)
	}
}

func TestWebhookNon2xxFails(t *testing.T) {
	for _, status := range []int{301, 400, 404, 500} {
		rec := &fakeReceiver{status: status, body: `{"detail":"` + testWebhookToken + `"}`}
		got := sendTo(t, rec, Channel{ID: "c", Kind: KindWebhook, Name: "n", URL: testWebhookURL}, false)
		if got.OK() {
			t.Fatalf("HTTP %d counted as delivered", status)
		}
		if !strings.Contains(got.Error, "HTTP") || strings.Contains(got.Error, testWebhookToken) {
			t.Errorf("HTTP %d: error = %q", status, got.Error)
		}
	}
}

func TestFeishuRequestAndSignature(t *testing.T) {
	rec := &fakeReceiver{body: `{"code":0,"msg":"success"}`}
	channel := Channel{ID: "c2", Kind: KindFeishu, Name: "team", URL: testFeishuURL, Secret: testSigningKey, Enabled: true}
	if got := sendTo(t, rec, channel, false); !got.OK() {
		t.Fatalf("delivery failed: %+v", got)
	}
	req := rec.all()[0]
	var body struct {
		MsgType   string `json:"msg_type"`
		Timestamp string `json:"timestamp"`
		Sign      string `json:"sign"`
		Content   struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.MsgType != "text" || !strings.Contains(body.Content.Text, "Account quota is almost used up") || !strings.Contains(body.Content.Text, "alice@example.com") {
		t.Errorf("body = %s", req.Body)
	}
	// Feishu's documented signature: Base64(HMAC-SHA256(key = "<ts>\n<secret>", data = "")).
	mac := hmac.New(sha256.New, []byte(body.Timestamp+"\n"+testSigningKey))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); body.Sign != want {
		t.Errorf("sign = %q, want %q", body.Sign, want)
	}
	if body.Timestamp != "1793620800" {
		t.Errorf("timestamp = %q", body.Timestamp)
	}

	// Without a secret the body carries no signature fields.
	rec = &fakeReceiver{body: `{"code":0}`}
	channel.Secret = ""
	if got := sendTo(t, rec, channel, false); !got.OK() {
		t.Fatalf("delivery failed: %+v", got)
	}
	if strings.Contains(string(rec.all()[0].Body), "sign") {
		t.Errorf("an unsigned bot got signature fields: %s", rec.all()[0].Body)
	}
}

func TestFeishuRefusalIsAFailureEvenOnHTTP200(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}`, "code 19021"},
		{`{"code":19024,"msg":"Key Words Not Found"}`, "Key Words Not Found"},
		{`{"StatusCode":19001,"StatusMessage":"param invalid"}`, "code 19001"},
		{`<html>captive portal</html>`, "not a bot reply"},
		{`{}`, "not a bot reply"},
	}
	for _, tt := range cases {
		rec := &fakeReceiver{body: tt.body}
		got := sendTo(t, rec, Channel{ID: "c", Kind: KindFeishu, Name: "n", URL: testFeishuURL}, false)
		if got.OK() || !strings.Contains(got.Error, tt.want) {
			t.Errorf("body %s: delivery = %+v, want a failure containing %q", tt.body, got, tt.want)
		}
		if strings.Contains(got.Error, testFeishuURL) || strings.Contains(got.Error, "0a1b2c3d") {
			t.Errorf("the error leaks the address: %q", got.Error)
		}
	}
}

func TestTelegramRequest(t *testing.T) {
	rec := &fakeReceiver{body: `{"ok":true,"result":{}}`}
	channel := Channel{ID: "c3", Kind: KindTelegram, Name: "phone", BotToken: testTelegramTok, ChatID: "-1001234567890", Enabled: true}
	if got := sendTo(t, rec, channel, false); !got.OK() {
		t.Fatalf("delivery failed: %+v", got)
	}
	req := rec.all()[0]
	if want := "https://api.telegram.org/bot" + testTelegramTok + "/sendMessage"; req.URL != want {
		t.Errorf("URL = %q, want %q", req.URL, want)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["chat_id"] != "-1001234567890" || !strings.Contains(body["text"].(string), "alice@example.com") {
		t.Errorf("body = %s", req.Body)
	}
	if _, markup := body["parse_mode"]; markup {
		t.Error("free text must not be sent with a parse mode")
	}
}

func TestTelegramRefusalNeverEchoesTheToken(t *testing.T) {
	rec := &fakeReceiver{status: 400, body: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`}
	got := sendTo(t, rec, Channel{ID: "c", Kind: KindTelegram, Name: "n", BotToken: testTelegramTok, ChatID: "-1"}, false)
	if got.OK() || !strings.Contains(got.Error, "chat not found") || strings.Contains(got.Error, testTelegramTok) {
		t.Errorf("delivery = %+v", got)
	}
}

func TestTransportErrorsNeverContainTheAddress(t *testing.T) {
	cases := map[string]error{
		"timeout":  &net.OpError{Op: "dial", Err: timeoutError{}},
		"refused":  &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")},
		"deadline": context.DeadlineExceeded,
		"dns":      &net.DNSError{Err: "no such host", Name: "open.feishu.cn"},
		"other":    errors.New(`Post "` + testFeishuURL + `": EOF`),
	}
	for name, cause := range cases {
		rec := &fakeReceiver{err: cause}
		got := sendTo(t, rec, Channel{ID: "c", Kind: KindFeishu, Name: "n", URL: testFeishuURL, Secret: testSigningKey}, false)
		if got.OK() || got.Error == "" {
			t.Errorf("%s: delivery = %+v", name, got)
		}
		for _, leak := range []string{testFeishuURL, "0a1b2c3d", testSigningKey, "open.feishu.cn", "Post"} {
			if strings.Contains(got.Error, leak) {
				t.Errorf("%s: the error leaks %q: %q", name, leak, got.Error)
			}
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestSendDeliversToEveryChannelAndSurvivesAPanic(t *testing.T) {
	rec := &fakeReceiver{body: `{"code":0}`}
	channels := []Channel{
		{ID: "a", Kind: KindWebhook, Name: "a", URL: testWebhookURL},
		{ID: "b", Kind: KindFeishu, Name: "b", URL: testFeishuURL},
		{ID: "c", Kind: "bogus", Name: "c"},
	}
	d := newDispatcherWith(rec)
	got := d.Send(context.Background(), channels, sampleMessage(), false)
	if len(got) != 3 || !got[0].OK() || !got[1].OK() || got[2].OK() {
		t.Fatalf("results = %+v", got)
	}
	if got[0].ChannelID != "a" || got[1].ChannelID != "b" || got[2].ChannelID != "c" {
		t.Errorf("results out of channel order: %+v", got)
	}

	panicking := roundTripFunc(func(*http.Request) (*http.Response, error) { panic("boom") })
	got = newDispatcherWith(panicking).Send(context.Background(), channels[:1], sampleMessage(), false)
	if got[0].OK() || got[0].Error != "internal error" {
		t.Errorf("a panicking transport must be a failed delivery: %+v", got[0])
	}
}

func TestDeliveryHonoursTheTimeout(t *testing.T) {
	slow := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(time.Minute):
			return respond(200, "{}"), nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := newDispatcherWith(slow).Send(ctx, []Channel{{ID: "a", Kind: KindWebhook, Name: "a", URL: testWebhookURL}}, sampleMessage(), false)
	if got[0].OK() || time.Since(started) > 5*time.Second {
		t.Fatalf("a slow receiver was not cut off: %+v after %s", got[0], time.Since(started))
	}
}
