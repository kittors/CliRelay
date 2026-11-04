package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type secretMaskDelayedStore struct {
	settings SecretMaskingSettings
	release  chan struct{}
}

func (s *secretMaskDelayedStore) Load(ctx context.Context, tenant string) (SecretMaskingSettings, int64, error) {
	select {
	case <-s.release:
		return s.settings, 1, nil
	case <-ctx.Done():
		return SecretMaskingSettings{}, 0, ctx.Err()
	}
}
func (s *secretMaskDelayedStore) Save(context.Context, string, SecretMaskingSettings, int64) error {
	return errors.New("unused")
}

func TestSecretMaskColdCacheFailureRefusesPlaintext(t *testing.T) {
	for _, mode := range []string{"healthy control", "read error", "slow read"} {
		t.Run(mode, func(t *testing.T) {
			persisted := newSecretStoreStub()
			first := NewSecretMaskingRegistry()
			first.SetStore(persisted)
			enabled, err := first.SetEnabled(context.Background(), "tenant-a", true)
			if err != nil {
				t.Fatal(err)
			}
			fresh := NewSecretMaskingRegistry()
			switch mode {
			case "read error":
				persisted.err = errors.New("database unavailable")
				fresh.SetStore(persisted)
			case "slow read":
				delayed := &secretMaskDelayedStore{settings: enabled, release: make(chan struct{})}
				fresh.SetStore(delayed)
				defer close(delayed.release)
			default:
				fresh.SetStore(persisted)
			}
			moderator := guardModerator(nil)
			moderator.secretMasking = fresh
			body := chatRequest("deploy with " + testOpenAIKey)
			req, opts := guardOptions("tenant-a", body)
			started := time.Now()
			result := moderator.GuardRequest(context.Background(), req, opts)
			outgoing := body
			if result.Payload != nil {
				outgoing = result.Payload
			}
			leaked := !result.Blocked && bytes.Contains(outgoing, []byte(testOpenAIKey))
			t.Logf("persisted enabled=%v blocked=%v plaintext retained=%v elapsed=%v", enabled.Enabled, result.Blocked, leaked, time.Since(started))
			if mode == "healthy control" {
				if leaked || result.Blocked || result.Payload == nil {
					t.Fatal("healthy control did not mask")
				}
			} else if leaked || !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable {
				t.Fatalf("unknown policy must refuse requests: %+v", result)
			}
		})
	}
}

func TestSecretMaskToolArgumentBusinessKeysAreMasked(t *testing.T) {
	for _, field := range []string{"command", "id", "type", "status", "signature", "encrypted_content"} {
		t.Run(field, func(t *testing.T) {
			value, _ := json.Marshal(map[string]string{field: testOpenAIKey})
			body := []byte(`{"model":"claude","tools":[{"name":"run","description":"test tool","input_schema":{"type":"object","additionalProperties":true}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"run","input":` + string(value) + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"go"}]}]}`)
			moderator := maskingModerator(t, "tenant-a")
			req, opts := guardOptions("tenant-a", body)
			opts.SourceFormat = sdktranslator.FormatClaude
			result := moderator.GuardRequest(context.Background(), req, opts)
			outgoing := body
			if result.Payload != nil {
				outgoing = result.Payload
			}
			leaked := !result.Blocked && bytes.Contains(outgoing, []byte(testOpenAIKey))
			t.Logf("field=%s blocked=%v plaintext retained=%v", field, result.Blocked, leaked)
			if leaked || result.Blocked {
				t.Fatal("ordinary tool data must be masked regardless of key name")
			}

		})
	}
}

func TestSecretMaskToolDescriptionsAreMasked(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"run","description":"Use ` + testOpenAIKey + ` to authenticate","parameters":{"type":"object","properties":{}}}}]}`)
	moderator := maskingModerator(t, "tenant-a")
	req, opts := guardOptions("tenant-a", body)
	result := moderator.GuardRequest(context.Background(), req, opts)
	outgoing := body
	if result.Payload != nil {
		outgoing = result.Payload
	}
	leaked := !result.Blocked && bytes.Contains(outgoing, []byte(testOpenAIKey))
	t.Logf("blocked=%v description plaintext retained=%v", result.Blocked, leaked)
	if leaked || result.Blocked {
		t.Fatal("tool description was not masked")
	}
}

func TestSecretMaskWarmPolicyFailureIsConservative(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached disabled", true: "cached enabled"}[enabled], func(t *testing.T) {
			store := newSecretStoreStub()
			registry := NewSecretMaskingRegistry()
			registry.SetStore(store)
			if _, err := registry.SetEnabled(context.Background(), "tenant-a", enabled); err != nil {
				t.Fatal(err)
			}
			store.err = errors.New("database unavailable")
			registry.Invalidate("tenant-a")
			moderator := guardModerator(nil)
			moderator.secretMasking = registry
			body := chatRequest("deploy with " + testOpenAIKey)
			req, opts := guardOptions("tenant-a", body)
			result := moderator.GuardRequest(context.Background(), req, opts)
			if enabled {
				if result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
					t.Fatal("last known enabled policy must keep masking")
				}
			} else if !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable {
				t.Fatal("stale disabled policy must not authorize plaintext")
			}
		})
	}
}

func TestSecretMaskArbitraryToolDataAcrossFormats(t *testing.T) {
	data := `{"id":"` + testOpenAIKey + `","signature":"` + testOpenAIKey + `","type":"image","content":"` + testOpenAIKey + `"}`
	for _, tc := range []struct {
		format sdktranslator.Format
		body   string
	}{
		{sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"run","arguments":` + quoteJSON(data) + `}}]}]}`},
		{sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"function_call","name":"run","arguments":` + quoteJSON(data) + `}]}`},
		{sdktranslator.FormatGemini, `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"run","args":` + data + `}}]}]}`},
		{sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"run","response":` + data + `}}]}]}`},
	} {
		table := testTable(2)
		masked, n, failure := maskRequestSecrets(tc.format, []byte(tc.body), table)
		if failure != "" || n != 3 || bytes.Contains(masked, []byte(testOpenAIKey)) || !gjson.ValidBytes(masked) {
			t.Fatalf("format=%v count=%d failure=%s", tc.format, n, failure)
		}
		if table.restore(string(masked)) != tc.body {
			t.Fatal("rewriting tool data changed protocol structure")
		}
	}
}

func TestSecretMaskDescriptionCoverageDoesNotChangeWordPolicies(t *testing.T) {
	for _, tc := range []struct {
		format sdktranslator.Format
		body   string
	}{
		{sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"run","description":"` + testOpenAIKey + `","parameters":{"type":"object","properties":{"arg":{"type":"string","description":"` + testOpenAIKey + `"}}}}}]}`},
		{sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"run","description":"` + testOpenAIKey + `","input_schema":{"type":"object"}}]}`},
		{sdktranslator.FormatGeminiCLI, `{"request":{"contents":[],"tools":[{"functionDeclarations":[{"name":"run","description":"` + testOpenAIKey + `"}]}]}}`},
	} {
		for _, unit := range extractWordUnits(tc.format, []byte(tc.body)) {
			if strings.Contains(unit.text, testOpenAIKey) {
				t.Fatal("secret-only descriptions changed word-policy coverage")
			}
		}
		table := testTable(3)
		masked, _, failure := maskRequestSecrets(tc.format, []byte(tc.body), table)
		if failure != "" || masked == nil || bytes.Contains(masked, []byte(testOpenAIKey)) {
			t.Fatalf("description not masked: format=%v failure=%s", tc.format, failure)
		}
		if table.restore(string(masked)) != tc.body {
			t.Fatal("schema/name structure changed")
		}
	}
}

type secretMaskRefreshStore struct {
	inner *secretStoreStub
	delay chan struct{}
}

func (s *secretMaskRefreshStore) Load(ctx context.Context, tenant string) (SecretMaskingSettings, int64, error) {
	if s.delay != nil {
		select {
		case <-s.delay:
		case <-ctx.Done():
			return SecretMaskingSettings{}, 0, ctx.Err()
		}
	}
	return s.inner.Load(ctx, tenant)
}
func (s *secretMaskRefreshStore) Save(ctx context.Context, tenant string, v SecretMaskingSettings, version int64) error {
	return s.inner.Save(ctx, tenant, v, version)
}

func TestSecretMaskWarmPolicyTimeoutIsConservative(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached disabled", true: "cached enabled"}[enabled], func(t *testing.T) {
			store := &secretMaskRefreshStore{inner: newSecretStoreStub()}
			registry := NewSecretMaskingRegistry()
			registry.SetStore(store)
			if _, err := registry.SetEnabled(context.Background(), "tenant-a", enabled); err != nil {
				t.Fatal(err)
			}
			store.delay = make(chan struct{})
			defer close(store.delay)
			registry.Invalidate("tenant-a")
			moderator := guardModerator(nil)
			moderator.secretMasking = registry
			body := chatRequest("deploy with " + testOpenAIKey)
			req, opts := guardOptions("tenant-a", body)
			result := moderator.GuardRequest(context.Background(), req, opts)
			if enabled {
				if result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
					t.Fatal("refresh timeout lost an enabled masking policy")
				}
			} else if !result.Blocked || result.HTTPStatus != http.StatusServiceUnavailable {
				t.Fatal("timed-out disabled policy authorized plaintext")
			}
		})
	}
}

func TestSecretMaskKnownDisabledAndAbsentStoreAllowRequests(t *testing.T) {
	for _, state := range []string{"no store", "missing row", "disabled row"} {
		t.Run(state, func(t *testing.T) {
			registry := NewSecretMaskingRegistry()
			if state != "no store" {
				registry.SetStore(newSecretStoreStub())
			}
			if state == "disabled row" {
				if _, err := registry.SetEnabled(context.Background(), "tenant-a", false); err != nil {
					t.Fatal(err)
				}
			}
			moderator := guardModerator(nil)
			moderator.secretMasking = registry
			body := chatRequest(testOpenAIKey)
			req, opts := guardOptions("tenant-a", body)
			result := moderator.GuardRequest(context.Background(), req, opts)
			if result.Blocked || result.Payload != nil || result.Reply != nil {
				t.Fatal("confirmed disabled policy changed the request")
			}
		})
	}
}

func TestSecretMaskPreservesSignedThinkingAndEnvelopeFields(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + testOpenAIKey + `","signature":"` + testOpenAIKey + `"},{"type":"tool_use","id":"` + testOpenAIKey + `","name":"run","input":{"id":"` + testOpenAIKey + `"}}]}]}`)
	table := testTable(4)
	masked, count, failure := maskRequestSecrets(sdktranslator.FormatClaude, body, table)
	if failure != "" || count != 1 {
		t.Fatalf("count=%d failure=%s", count, failure)
	}
	for _, path := range []string{"messages.0.content.0.thinking", "messages.0.content.0.signature", "messages.0.content.1.id"} {
		if gjson.GetBytes(masked, path).String() != testOpenAIKey {
			t.Fatalf("changed signed/protocol field %s", path)
		}
	}
	if gjson.GetBytes(masked, "messages.0.content.1.input.id").String() == testOpenAIKey {
		t.Fatal("business id not masked")
	}
	gemini := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"` + testOpenAIKey + `","thoughtSignature":"signature"},{"text":"` + testOpenAIKey + `"}]}]}`)
	masked, count, failure = maskRequestSecrets(sdktranslator.FormatGemini, gemini, testTable(5))
	if failure != "" || count != 1 || gjson.GetBytes(masked, "contents.0.parts.0.text").String() != testOpenAIKey || gjson.GetBytes(masked, "contents.0.parts.0.thoughtSignature").String() != "signature" {
		t.Fatal("Gemini thought protection changed")
	}
}

func TestSecretMaskUnknownPolicyStaysBlockedUntilRecovery(t *testing.T) {
	store := newSecretStoreStub()
	writer := NewSecretMaskingRegistry()
	writer.SetStore(store)
	if _, err := writer.SetEnabled(context.Background(), "tenant-a", true); err != nil {
		t.Fatal(err)
	}
	registry := NewSecretMaskingRegistry()
	registry.SetStore(store)
	store.err = errors.New("database unavailable")
	moderator := guardModerator(nil)
	moderator.secretMasking = registry
	req, opts := guardOptions("tenant-a", chatRequest(testOpenAIKey))
	for i := 0; i < 2; i++ {
		if result := moderator.GuardRequest(context.Background(), req, opts); !result.Blocked {
			t.Fatal("error cache turned an unknown policy off")
		}
	}
	loads := store.loads
	if _, err := registry.For("tenant-a"); err == nil || store.loads != loads {
		t.Fatal("error cache must preserve unknown state without repeated reads")
	}
	store.err = nil
	registry.Invalidate("tenant-a")
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
		t.Fatal("invalidation did not recover enabled policy")
	}
	store.err = errors.New("database unavailable")
	registry.Invalidate("tenant-a")
	if _, err := registry.For("tenant-a"); err == nil {
		t.Fatal("expected failed refresh")
	}
	store.err = nil
	if _, err := registry.SetEnabled(context.Background(), "tenant-a", false); err != nil {
		t.Fatal(err)
	}
	if result := moderator.GuardRequest(context.Background(), req, opts); result.Blocked || result.Payload != nil {
		t.Fatal("confirmed disabled write did not clear cached failure")
	}
}

func TestSecretMaskLegitimateToolResultsAcrossFormats(t *testing.T) {
	data := `{"id":"` + testOpenAIKey + `","signature":"` + testOpenAIKey + `","type":"image","content":"` + testOpenAIKey + `"}`
	for _, tc := range []struct {
		format sdktranslator.Format
		body   string
	}{
		{sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + quoteJSON(data) + `}]}]}`},
		{sdktranslator.FormatOpenAI, `{"messages":[{"role":"tool","tool_call_id":"t1","content":` + quoteJSON(data) + `}]}`},
		{sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"function_call_output","call_id":"t1","output":` + quoteJSON(data) + `}]}`},
		{sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"run","response":` + data + `}}]}]}`},
		{sdktranslator.FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"run","input":` + data + `}]}]}`},
	} {
		table := testTable(6)
		masked, n, failure := maskRequestSecrets(tc.format, []byte(tc.body), table)
		if failure != "" || n != 3 || bytes.Contains(masked, []byte(testOpenAIKey)) || !gjson.ValidBytes(masked) {
			t.Fatalf("format=%v count=%d failure=%s", tc.format, n, failure)
		}
		if table.restore(string(masked)) != tc.body {
			t.Fatal("tool result protocol structure changed")
		}
	}
}
