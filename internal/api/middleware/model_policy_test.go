package middleware

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

const policyTestKey = "sk-policy-test"

// installPolicy 发布一份只含 policyTestKey 的策略表，并注入按模型返回用量的快照函数，
// 返回快照被读取的次数。
func installPolicy(t *testing.T, policy *accesspolicy.Policy, account, key map[string]quota.Matrix, accountTotal, keyTotal quota.Matrix) (*atomic.Int64, *atomic.Int64) {
	t.Helper()
	resetQuotaMiddlewareState(t)
	accesspolicy.Publish(map[string]*accesspolicy.Policy{policyTestKey: policy})
	prevAccount, prevKey := queryAccountSnapshotFunc, queryKeySnapshotFunc
	accountReads, keyReads := &atomic.Int64{}, &atomic.Int64{}
	queryAccountSnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		accountReads.Add(1)
		return quota.UsageSnapshot{Total: accountTotal, Models: account}, nil
	}
	queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		keyReads.Add(1)
		return quota.UsageSnapshot{Total: keyTotal, Models: key}, nil
	}
	t.Cleanup(func() {
		accesspolicy.Publish(nil)
		queryAccountSnapshotFunc, queryKeySnapshotFunc = prevAccount, prevKey
	})
	return accountReads, keyReads
}

// policyRouter 搭一条与网关相同顺序的链：鉴权元数据 → QuotaMiddleware → ModelPolicyMiddleware → handler。
// handler 回显它看到的模型（请求体或 Gemini 路径）。
func policyRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", policyTestKey)
		c.Set("accessMetadata", map[string]string{"end-user-id": "user-1", "tenant-id": "tenant-1", "api-key-id": "key-1"})
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.Use(ModelPolicyMiddleware(func(_ *gin.Context, model string) string { return model }))
	echo := func(c *gin.Context) {
		var body struct {
			Model string `json:"model"`
		}
		raw, _ := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
		_ = json.Unmarshal(raw, &body)
		c.JSON(http.StatusOK, gin.H{"model": body.Model, "action": c.Param("action")})
	}
	router.POST("/v1/chat/completions", echo)
	router.POST("/v1beta/models/*action", echo)
	return router
}

func postModel(router *gin.Engine, path, model string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func decodeEcho(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	raw, _ := io.ReadAll(rec.Body)
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func costWeek(v float64) quota.Matrix {
	m := quota.Matrix{}
	m.Set(quota.MetricCost, quota.PeriodWeek, v)
	return m
}

func TestModelPolicyRoutesTheRequestModel(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		Routes: []quota.ModelRoute{{ID: "down", From: []string{"claude-opus-*"}, To: "claude-sonnet-4-5"}},
	}, nil, nil, quota.Matrix{}, quota.Matrix{})
	rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if got := decodeEcho(t, rec)["model"]; got != "claude-sonnet-4-5" {
		t.Fatalf("handler saw model %q, want the routed model", got)
	}
	if rec.Header().Get("X-CliRelay-Model-Requested") != "claude-opus-4-5" || rec.Header().Get("X-CliRelay-Model-Routed") != "claude-sonnet-4-5" {
		t.Fatalf("route headers = %v", rec.Header())
	}
}

func TestModelPolicyRoutesGeminiPathModel(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		Routes: []quota.ModelRoute{{ID: "flash", From: []string{"gemini-2.5-pro"}, To: "gemini-2.5-flash"}},
	}, nil, nil, quota.Matrix{}, quota.Matrix{})
	rec := postModel(policyRouter(), "/v1beta/models/gemini-2.5-pro:generateContent", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if got := decodeEcho(t, rec)["action"]; got != "/gemini-2.5-flash:generateContent" {
		t.Fatalf("handler saw action %q", got)
	}
}

func TestModelPolicyRejectsExceededRuleWithStructuredError(t *testing.T) {
	accountReads, keyReads := installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		AccountRules: []quota.ModelRule{{ID: "opus", Name: "Opus", Models: []string{"claude-opus-*"}, Limits: costWeek(100)}},
	}, map[string]quota.Matrix{"claude-opus-4-5": costWeek(100)}, nil, quota.Matrix{}, quota.Matrix{})
	rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"code": "model_quota_exceeded", "type": "rate_limit_exceeded", "scope": "account", "period": "week",
		"metric": "cost", "model": "claude-opus-4-5", "rule": "opus", "rule_name": "Opus",
	} {
		if body.Error[field] != want {
			t.Fatalf("error.%s = %v, want %v (body %s)", field, body.Error[field], want, rec.Body)
		}
	}
	if rec.Header().Get("X-CliRelay-Quota-Rule") != "opus" || rec.Header().Get("X-CliRelay-Quota-Rejected-By") != "model_quota" {
		t.Fatalf("headers = %v", rec.Header())
	}
	if accountReads.Load() != 1 || keyReads.Load() != 0 {
		t.Fatalf("snapshot reads account=%d key=%d, want 1/0", accountReads.Load(), keyReads.Load())
	}
}

func TestModelPolicyFallsBackInsteadOfRejecting(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		AccountRules: []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: costWeek(100), FallbackModel: "claude-sonnet-4-5"}},
	}, map[string]quota.Matrix{"claude-opus-4-5": costWeek(100)}, nil, quota.Matrix{}, quota.Matrix{})
	rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if got := decodeEcho(t, rec)["model"]; got != "claude-sonnet-4-5" {
		t.Fatalf("model = %q, want fallback", got)
	}
	if rec.Header().Get("X-CliRelay-Model-Fallback") != "opus" {
		t.Fatalf("fallback header = %q", rec.Header().Get("X-CliRelay-Model-Fallback"))
	}
}

// 总额度矩阵里的请求次数格子由 QuotaMiddleware 检查；同一个请求里模型策略复用它读到的快照。
func TestPolicyCellsAreCheckedOnceAndShareTheSnapshot(t *testing.T) {
	weekRequests := quota.Matrix{}
	weekRequests.Set(quota.MetricRequests, quota.PeriodWeek, 50)
	used := quota.Matrix{}
	used.Set(quota.MetricRequests, quota.PeriodWeek, 10)
	accountReads, _ := installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		AccountLimits: weekRequests,
		AccountRules:  []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: costWeek(100)}},
	}, map[string]quota.Matrix{"claude-opus-4-5": costWeek(1)}, nil, used, quota.Matrix{})
	rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if accountReads.Load() != 1 {
		t.Fatalf("account snapshot read %d times in one request, want 1", accountReads.Load())
	}

	used.Set(quota.MetricRequests, quota.PeriodWeek, 50)
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1", AccountLimits: weekRequests,
	}, nil, nil, used, quota.Matrix{})
	rec = postModel(policyRouter(), "/v1/chat/completions", "gpt-5")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-CliRelay-Quota-Code") != "request_limit_exceeded" ||
		rec.Header().Get("X-CliRelay-Quota-Metric") != "requests" || rec.Header().Get("X-CliRelay-Quota-Scope") != "account" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestModelPolicyFailsClosedWhenUsageIsUnavailable(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		KeyRules: []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: costWeek(100)}},
	}, nil, nil, quota.Matrix{}, quota.Matrix{})
	queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) { return quota.UsageSnapshot{}, errors.New("db down") }
	rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "quota_usage_unavailable") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	// 不命中规则的模型不读用量，也就不受数据库故障影响。
	rec = postModel(policyRouter(), "/v1/chat/completions", "gpt-5")
	if rec.Code != http.StatusOK {
		t.Fatalf("unmatched model status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestModelPolicyTurnGateRoutesAndRejects(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		Routes:   []quota.ModelRoute{{ID: "r", From: []string{"gpt-5"}, To: "gpt-5-mini"}},
		KeyRules: []quota.ModelRule{{ID: "mini", Models: []string{"gpt-5-mini"}, Limits: costWeek(5)}},
	}, nil, map[string]quota.Matrix{"gpt-5-mini": costWeek(1)}, quota.Matrix{}, quota.Matrix{})
	gate := modelPolicyTurnGate(accesspolicy.Lookup(policyTestKey), nil)
	if model, rejection := gate("gpt-5"); rejection != nil || model != "gpt-5-mini" {
		t.Fatalf("turn = %q, %+v", model, rejection)
	}
	queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		return quota.UsageSnapshot{Models: map[string]quota.Matrix{"gpt-5-mini": costWeek(5)}}, nil
	}
	if _, rejection := gate("gpt-5"); rejection == nil || rejection.StatusCode != http.StatusTooManyRequests ||
		!strings.Contains(string(rejection.Body), "model_quota_exceeded") {
		t.Fatalf("rejection = %+v", rejection)
	}
}

func TestModelPolicySkipsKeysWithoutModelPolicy(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{TenantID: "tenant-1", APIKeyID: "key-1"}, nil, nil, quota.Matrix{}, quota.Matrix{})
	rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
	if rec.Code != http.StatusOK || decodeEcho(t, rec)["model"] != "claude-opus-4-5" || rec.Header().Get("X-CliRelay-Model-Routed") != "" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestHeaderSafeDropsControlCharacters(t *testing.T) {
	if got := headerSafe("gpt\r\nX-Evil: 1"); got != "gptX-Evil: 1" {
		t.Fatalf("headerSafe = %q", got)
	}
}
