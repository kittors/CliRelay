package aiaccountstatus

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestZAICodingQuotaURL(t *testing.T) {
	for _, tc := range []struct {
		name, base, want string
	}{
		{"global", "https://api.z.ai/api/coding/paas/v4", "https://api.z.ai/api/monitor/usage/quota/limit"},
		{"trailing slash", "https://api.z.ai/api/coding/paas/v4/", "https://api.z.ai/api/monitor/usage/quota/limit"},
		{"china", "https://open.bigmodel.cn/api/coding/paas/v4", "https://open.bigmodel.cn/api/monitor/usage/quota/limit"},
		{"other provider", "https://example.com/api/coding/paas/v4", ""},
		{"host suffix", "https://api.z.ai.example.com/api/coding/paas/v4", ""},
		{"plain HTTP", "http://api.z.ai/api/coding/paas/v4", ""},
		{"ordinary API", "https://api.z.ai/api/paas/v4", ""},
		{"userinfo", "https://user@api.z.ai/api/coding/paas/v4", ""},
		{"custom port", "https://api.z.ai:8443/api/coding/paas/v4", ""},
		{"query", "https://api.z.ai/api/coding/paas/v4?key=example", ""},
		{"fragment", "https://api.z.ai/api/coding/paas/v4#fragment", ""},
		{"invalid URL", "https://[", ""},
		{"missing URL", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// OpenAI-compatible providers may have any configured name.
			auth := &coreauth.Auth{Provider: "custom-coding-name", Attributes: map[string]string{"base_url": tc.base}}
			if got := zaiCodingQuotaURL(auth); got != tc.want {
				t.Fatalf("quota endpoint = %q, want %q", got, tc.want)
			}
		})
	}
	if got := zaiCodingQuotaURL(nil); got != "" {
		t.Fatalf("nil auth endpoint = %q", got)
	}
}

func TestProbeAuthDispatchesZAICodingQuota(t *testing.T) {
	for _, host := range []string{"api.z.ai", "open.bigmodel.cn"} {
		t.Run(host, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.Host != host || r.URL.Path != "/api/monitor/usage/quota/limit" {
					t.Errorf("unexpected request: %s %s %s", r.Method, r.Host, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer dummy-coding-credential" || r.Header.Get("Accept") != "application/json" {
					t.Error("missing resolved credential or JSON Accept header")
				}
				_, _ = io.WriteString(w, `{"code":200,"success":true,"data":{"level":"pro","limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":20}]}}`)
			}))
			defer upstream.Close()
			// Tunnel only to the local fixture. This exercises the configured account
			// proxy and the real HTTPS request without contacting either provider.
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != host+":443" {
					t.Errorf("unexpected proxy target: %s %s", r.Method, r.Host)
					http.Error(w, "unexpected target", http.StatusBadGateway)
					return
				}
				remote, err := net.DialTimeout("tcp", upstream.Listener.Addr().String(), time.Second)
				if err != nil {
					t.Error(err)
					http.Error(w, "fixture unavailable", http.StatusBadGateway)
					return
				}
				defer remote.Close()
				client, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer client.Close()
				_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(remote, client)
					_ = remote.Close()
					close(done)
				}()
				_, _ = io.Copy(client, remote)
				_ = client.Close()
				<-done
			}))
			defer proxy.Close()
			cfg := &config.Config{SDKConfig: config.SDKConfig{InsecureSkipVerify: true}} // local TLS fixture
			auth := &coreauth.Auth{Provider: "custom-coding-name", ProxyURL: proxy.URL, Attributes: map[string]string{
				"base_url": "https://" + host + "/api/coding/paas/v4", "api_key": "dummy-coding-credential",
			}}
			svc := managementapitools.New(cfg, nil, managementapitools.Dependencies{})
			defer svc.APICallTransport(auth).(*http.Transport).CloseIdleConnections()
			result, err := probeAuth(context.Background(), svc, cfg, auth)
			if err != nil || result.Unsupported || result.PlanType != "pro" || len(result.Quotas) != 1 || requests.Load() != 1 {
				t.Fatalf("result=%+v requests=%d err=%v", result, requests.Load(), err)
			}
			if q := result.Quotas[0]; q.QuotaKey != "code_5h" || q.Percent == nil || *q.Percent != 80 {
				t.Fatalf("unexpected quota: %+v", q)
			}
		})
	}
	auth := &coreauth.Auth{Provider: "zai-coding", Attributes: map[string]string{"base_url": "https://example.com/v1"}}
	result, err := probeAuth(context.Background(), nil, &config.Config{}, auth)
	if err != nil || !result.Unsupported {
		t.Fatalf("display name must not select Z.ai's endpoint: result=%+v err=%v", result, err)
	}
}

func TestParseZAICodingQuotaWindows(t *testing.T) {
	result, err := parseZAICodingQuota([]byte(`{
		"code":200,"success":true,"data":{"level":"pro","limits":[
			{"type":"CREDIT_LIMIT","unit":6,"number":1,"percentage":8.25,"nextResetTime":1900000123456},
			{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":3,"nextResetTime":1899990123456},
			{"type":"TIME_LIMIT","percentage":15},
			{"type":"CREDIT_LIMIT","unit":3,"number":24,"percentage":50}
		]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.PlanType != "pro" || len(result.Quotas) != 2 {
		t.Fatalf("unexpected plan/windows: %+v", result)
	}
	for _, tc := range []struct {
		key, label           string
		seconds, resetMillis int64
		remaining            float64
	}{
		{"code_5h", "m_quota.code_5h", 18000, 1899990123456, 97},
		{"code_week", "m_quota.code_weekly", 604800, 1900000123456, 91.75},
	} {
		q := quotaByKey(result.Quotas, tc.key)
		if q == nil || q.Percent == nil || *q.Percent != tc.remaining || q.WindowSeconds != tc.seconds || q.QuotaLabel != tc.label {
			t.Fatalf("%s: got %+v, want %.2f%% remaining over %ds", tc.key, q, tc.remaining, tc.seconds)
		}
		if q.ResetAt == nil || q.ResetAt.UnixMilli() != tc.resetMillis || q.ResetAt.Location() != time.UTC {
			t.Fatalf("%s: reset=%v, want exact epoch milliseconds %d in UTC", tc.key, q.ResetAt, tc.resetMillis)
		}
	}
}

func TestParseZAICodingQuotaLegacyAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, limit string
		remaining   float64
	}{
		{"legacy unused", `{"type":"TOKENS_LIMIT","percentage":0}`, 100},
		{"exhausted", `{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":100}`, 0},
		{"legacy explicit window", `{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":25}`, 75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"code":200,"success":true,"data":{"limits":[%s]}}`, tc.limit)
			result, err := parseZAICodingQuota([]byte(body))
			if err != nil || len(result.Quotas) != 1 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			q := result.Quotas[0]
			if q.QuotaKey != "code_5h" || q.Percent == nil || *q.Percent != tc.remaining || q.ResetAt != nil {
				t.Fatalf("unexpected quota or invented reset time: %+v", q)
			}
		})
	}
}

func TestParseZAICodingQuotaRejectsInvalidResponses(t *testing.T) {
	validData := `{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":0}]}`
	for _, tc := range []struct{ name, body string }{
		{"invalid JSON", `{"code":200`},
		{"upstream failure", `{"code":401,"success":true,"data":` + validData + `}`},
		{"missing result code", `{"success":true,"data":` + validData + `}`},
		{"string result code", `{"code":"200","success":true,"data":` + validData + `}`},
		{"fractional result code", `{"code":200.5,"success":true,"data":` + validData + `}`},
		{"failed success flag", `{"code":200,"success":false,"data":` + validData + `}`},
		{"missing success flag", `{"code":200,"data":` + validData + `}`},
		{"missing limits", `{"code":200,"success":true,"data":{}}`},
		{"null limits", `{"code":200,"success":true,"data":{"limits":null}}`},
		{"empty limits", `{"code":200,"success":true,"data":{"limits":[]}}`},
		{"unknown windows", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":24,"percentage":3}]}}`},
		{"missing window", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","percentage":3}]}}`},
		{"fractional unit", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3.5,"number":5,"percentage":3}]}}`},
		{"fractional window", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":6,"number":1.5,"percentage":3}]}}`},
		{"missing percentage", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5}]}}`},
		{"null percentage", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":null}]}}`},
		{"string percentage", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":"3"}]}}`},
		{"negative percentage", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":-1}]}}`},
		{"overused percentage", `{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":101}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseZAICodingQuota([]byte(tc.body))
			if err == nil || len(result.Quotas) != 0 {
				t.Fatalf("invalid response became usable quota: result=%+v err=%v", result, err)
			}
		})
	}
}

func FuzzZAICodingQuotaPercentAndReset(f *testing.F) {
	f.Add(uint16(0), uint32(0), false)
	f.Add(uint16(10000), uint32(1), true)
	f.Add(uint16(325), uint32(123456), true)
	f.Add(uint16(10001), uint32(999), false)
	f.Fuzz(func(t *testing.T, usedBasisPoints uint16, resetOffset uint32, weekly bool) {
		used := float64(usedBasisPoints) / 100
		resetMillis := int64(1800000000000) + int64(resetOffset)
		unit, number, key := 3, 5, "code_5h"
		if weekly {
			unit, number, key = 6, 1, "code_week"
		}
		body := fmt.Sprintf(`{"code":200,"success":true,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":%d,"number":%d,"percentage":%.2f,"nextResetTime":%d}]}}`, unit, number, used, resetMillis)
		result, err := parseZAICodingQuota([]byte(body))
		if usedBasisPoints > 10000 {
			if err == nil {
				t.Fatal("out-of-range usage accepted")
			}
			return
		}
		if err != nil || len(result.Quotas) != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		q := result.Quotas[0]
		if q.QuotaKey != key || q.Percent == nil || math.Abs(*q.Percent+used-100) > 1e-9 {
			t.Fatalf("remaining + used must equal 100 for %s: %+v", key, q)
		}
		if q.ResetAt == nil || q.ResetAt.UnixMilli() != resetMillis {
			t.Fatalf("reset milliseconds changed: %v, want %d", q.ResetAt, resetMillis)
		}
	})
}

func TestProbeZAICodingDoesNotReturnUpstreamErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider echoed dummy-coding-credential", http.StatusUnauthorized)
	}))
	defer server.Close()
	auth := &coreauth.Auth{Attributes: map[string]string{"api_key": "dummy-coding-credential"}}
	svc := managementapitools.New(&config.Config{}, nil, managementapitools.Dependencies{})
	_, err := probeZAICoding(context.Background(), svc, auth, server.URL)
	if err == nil || strings.Contains(err.Error(), "dummy-coding-credential") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unexpected upstream error projection: %v", err)
	}
}
