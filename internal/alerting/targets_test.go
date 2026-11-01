package alerting

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestValidateWebhookURL(t *testing.T) {
	tests := []struct {
		name         string
		url          string
		allowPrivate bool
		wantErr      string
	}{
		{"public https", "https://hooks.example.com/alerts/abc", false, ""},
		{"public https with port", "https://hooks.example.com:8443/x", false, ""},
		{"empty", "", false, "is required"},
		{"plain http for a tenant", "http://hooks.example.com/x", false, "must use https"},
		{"plain http for the operator", "http://hooks.example.com/x", true, ""},
		{"other scheme", "ftp://hooks.example.com/x", false, "http or https"},
		{"no host", "https:///x", false, "full URL"},
		{"credentials in the url", "https://user:pw@hooks.example.com/x", false, "user name or password"},
		{"space in the url", "https://hooks.example.com/a b", false, "spaces"},
		{"bad port", "https://hooks.example.com:99999/x", false, "invalid port"},
		{"loopback literal", "https://127.0.0.1/x", false, "private or internal"},
		{"private literal", "https://10.1.2.3/x", false, "private or internal"},
		{"link local literal (metadata service)", "https://169.254.169.254/latest", false, "private or internal"},
		{"ipv6 loopback", "https://[::1]/x", false, "private or internal"},
		{"ipv6 unique local", "https://[fd00::1]/x", false, "private or internal"},
		{"ipv4 mapped private", "https://[::ffff:10.0.0.1]/x", false, "private or internal"},
		{"public literal", "https://8.8.8.8/x", false, ""},
		{"localhost", "https://localhost/x", false, "private or internal"},
		{"subdomain of localhost", "https://a.localhost/x", false, "private or internal"},
		{"single label host", "https://relay/x", false, "private or internal"},
		{"internal suffix", "https://relay.internal/x", false, "private or internal"},
		{"mdns", "https://printer.local/x", false, "private or internal"},
		{"operator may use the local network", "http://127.0.0.1:9000/hook", true, ""},
		{"operator may use a private literal", "https://10.0.0.5/hook", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWebhookURL(tt.url, tt.allowPrivate)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateWebhookURL(%q) = %v, want nil", tt.url, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateWebhookURL(%q) = %v, want an error containing %q", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestValidateFeishuURL(t *testing.T) {
	good := []string{
		"https://open.feishu.cn/open-apis/bot/v2/hook/0a1b2c3d-4e5f-6789-abcd-ef0123456789",
		"https://open.larksuite.com/open-apis/bot/v2/hook/abcdefgh12345678",
		"https://OPEN.FEISHU.CN/open-apis/bot/v2/hook/abcdefgh12345678",
	}
	for _, raw := range good {
		if err := ValidateFeishuURL(raw); err != nil {
			t.Errorf("ValidateFeishuURL(%q) = %v, want nil", raw, err)
		}
	}
	bad := []string{
		"",
		"http://open.feishu.cn/open-apis/bot/v2/hook/abcdefgh12345678",
		"https://evil.example.com/open-apis/bot/v2/hook/abcdefgh12345678",
		"https://open.feishu.cn.evil.example.com/open-apis/bot/v2/hook/abcdefgh12345678",
		"https://open.feishu.cn/open-apis/bot/v2/hook/",
		"https://open.feishu.cn/open-apis/bot/v2/hook/short",
		"https://open.feishu.cn/open-apis/bot/v2/hook/abcdefgh12345678?x=1",
		"https://open.feishu.cn/somewhere/else",
		"https://127.0.0.1/open-apis/bot/v2/hook/abcdefgh12345678",
	}
	for _, raw := range bad {
		if err := ValidateFeishuURL(raw); err == nil {
			t.Errorf("ValidateFeishuURL(%q) = nil, want an error", raw)
		}
	}
}

func TestValidateTelegram(t *testing.T) {
	const token = "123456789:AAH-ExampleTokenExampleTokenExample_1"
	if err := ValidateTelegramToken(token); err != nil {
		t.Errorf("ValidateTelegramToken(valid) = %v", err)
	}
	for _, bad := range []string{"", "123456789", "abc:def", token + "/../x", token + "?x=1", " " + token, "123456789:short"} {
		if err := ValidateTelegramToken(bad); err == nil {
			t.Errorf("ValidateTelegramToken(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"-1001234567890", "123456", "@alerts_channel"} {
		if err := ValidateTelegramChatID(good); err != nil {
			t.Errorf("ValidateTelegramChatID(%q) = %v", good, err)
		}
	}
	for _, bad := range []string{"", "abc", "@x", "12 34", "-", "@1abcde"} {
		if err := ValidateTelegramChatID(bad); err == nil {
			t.Errorf("ValidateTelegramChatID(%q) = nil, want an error", bad)
		}
	}
}

func TestAddrBlocked(t *testing.T) {
	blocked := []string{"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "::", "::1", "fc00::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "2001:db8::1"}
	for _, s := range blocked {
		if !addrBlocked(netip.MustParseAddr(s)) {
			t.Errorf("addrBlocked(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1", "100.63.255.255"} {
		if addrBlocked(netip.MustParseAddr(s)) {
			t.Errorf("addrBlocked(%s) = true, want false", s)
		}
	}
}

// The guarded client refuses a loopback receiver even when the URL passed
// validation (a host name that resolves to a private address), while the
// operator's client reaches it.
func TestDirectClientRefusesPrivateAddresses(t *testing.T) {
	reached := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	channel := Channel{ID: "c", Kind: KindWebhook, Name: "n", URL: server.URL + "/hook", Enabled: true}
	msg := testMessage("tenant", LanguageEN, timeNow(), []Channel{channel})

	d := NewDispatcher()
	got := d.Send(context.Background(), []Channel{channel}, msg, false)
	if len(got) != 1 || got[0].OK() {
		t.Fatalf("a tenant's webhook to a loopback address was delivered: %+v", got)
	}
	if got[0].Error != errBlockedAddress.Error() {
		t.Errorf("Error = %q, want %q", got[0].Error, errBlockedAddress.Error())
	}
	if reached != 0 {
		t.Fatalf("the receiver was reached %d time(s)", reached)
	}

	got = d.Send(context.Background(), []Channel{channel}, msg, true)
	if !got[0].OK() || reached != 1 {
		t.Fatalf("the operator's webhook to a local receiver failed: %+v (reached %d)", got[0], reached)
	}
}

func TestPublicOnlyControl(t *testing.T) {
	if err := publicOnlyControl("tcp", "127.0.0.1:80", nil); !errors.Is(err, errBlockedAddress) {
		t.Errorf("loopback: err = %v", err)
	}
	if err := publicOnlyControl("tcp", "[::1]:80", nil); !errors.Is(err, errBlockedAddress) {
		t.Errorf("ipv6 loopback: err = %v", err)
	}
	if err := publicOnlyControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("public address: err = %v", err)
	}
	if err := publicOnlyControl("tcp", "not an address", nil); !errors.Is(err, errBlockedAddress) {
		t.Errorf("garbage: err = %v", err)
	}
}
