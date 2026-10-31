package management

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

func reviewByIndex(t *testing.T, body map[string]any) map[int]map[string]any {
	t.Helper()
	out := map[int]map[string]any{}
	items, _ := body["reviews"].([]any)
	for _, item := range items {
		review := item.(map[string]any)
		out[int(review["index"].(float64))] = review
	}
	return out
}

func TestWordPolicyValidateReportsWhatAnImportWouldDo(t *testing.T) {
	h := setupWordPolicyTest(t)
	rec, _ := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[{"id":"r-alpha","pattern":"Alpha"},{"id":"r-card","kind":"regex","pattern":"\\d{4}"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	body := `{"rules":[
		{"pattern":"fresh"},
		{"pattern":"  FRESH "},
		{"pattern":"alpha"},
		{"kind":"regex","pattern":"\\d{4}"},
		{"kind":"regex","pattern":"("},
		{"pattern":""},
		{"kind":"regex","pattern":"a*"},
		{"kind":"glob","pattern":"x"},
		{"kind":"regex","pattern":"new-regex"}]}`
	rec, got := wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, body, "")
	wordExpectStatus(t, rec, http.StatusOK)
	summary := got["summary"].(map[string]any)
	want := map[string]float64{"total": 9, "new": 2, "duplicate": 1, "existing": 2, "invalid": 4, "existing_rules": 2, "existing_regex": 1, "new_regex": 1}
	for key, value := range want {
		if summary[key] != value {
			t.Errorf("summary[%s] = %v, want %v (%v)", key, summary[key], value, summary)
		}
	}
	if summary["fits"] != true {
		t.Fatalf("summary = %v", summary)
	}

	reviews := reviewByIndex(t, got)
	// Rules that are fine and new are not listed: the panel has them in its file.
	for _, index := range []int{0, 8} {
		if _, listed := reviews[index]; listed {
			t.Errorf("rule %d is new and valid but was listed: %v", index, reviews[index])
		}
	}
	statuses := map[int]string{1: "duplicate", 2: "existing", 3: "existing", 4: "invalid", 5: "invalid", 6: "invalid", 7: "invalid"}
	for index, status := range statuses {
		if reviews[index]["status"] != status {
			t.Errorf("rule %d: %v, want %s", index, reviews[index], status)
		}
	}
	if reviews[1]["duplicate_of"] != float64(0) || reviews[2]["existing_id"] != "r-alpha" || reviews[3]["existing_id"] != "r-card" {
		t.Errorf("references: %v %v %v", reviews[1], reviews[2], reviews[3])
	}
	codes := map[int]string{4: "invalid_regex", 5: "empty", 6: "matches_empty", 7: "unknown_kind"}
	for index, code := range codes {
		problem, _ := reviews[index]["problem"].(map[string]any)
		if problem["code"] != code || problem["message"] == "" || problem["index"] != float64(index) {
			t.Errorf("rule %d: problem %v, want code %s", index, problem, code)
		}
	}

	// Nothing was stored.
	_, stored := wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
	if stored["version"] != float64(1) || len(wordRuleList(t, stored)) != 2 {
		t.Fatalf("validate changed the policy: %v", stored)
	}
}

func TestWordPolicyValidateSaysWhetherTheRulesFit(t *testing.T) {
	h := setupWordPolicyTest(t)
	regexes := make([]string, 0, contentmoderation.MaxWordRegexPerPolicy)
	for i := 0; i < contentmoderation.MaxWordRegexPerPolicy; i++ {
		regexes = append(regexes, fmt.Sprintf(`{"kind":"regex","pattern":"word%d"}`, i))
	}
	rec, _ := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[`+strings.Join(regexes, ",")+`]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	_, got := wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"rules":[{"pattern":"plain"}]}`, "")
	if got["summary"].(map[string]any)["fits"] != true {
		t.Fatalf("a keyword fits beside a full set of regular expressions: %v", got["summary"])
	}
	_, got = wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"rules":[{"kind":"regex","pattern":"one-too-many"}]}`, "")
	if summary := got["summary"].(map[string]any); summary["fits"] != false || summary["new"] != float64(1) {
		t.Fatalf("a regular expression beyond the limit: %v", summary)
	}

	rules := make([]string, 0, 2*contentmoderation.MaxWordRulesPerPolicy+1)
	for i := 0; i <= 2*contentmoderation.MaxWordRulesPerPolicy; i++ {
		rules = append(rules, fmt.Sprintf(`{"pattern":"k%d"}`, i))
	}
	rec, got = wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"rules":[`+strings.Join(rules, ",")+`]}`, "")
	wordExpectStatus(t, rec, http.StatusUnprocessableEntity)
	if wordErrorCode(got) != "word_policy_invalid" {
		t.Fatalf("body = %v", got)
	}
}

func TestWordPolicyValidateScopes(t *testing.T) {
	h := setupWordPolicyTest(t)
	createPolicyKeyFor(t, wordTenantA)
	rec, _ := wordTenantAUser.call(h.PutWordPolicyKey, http.MethodPut, `{"rules":[{"id":"k-rule","pattern":"keyrule"}]}`, "key-1")
	wordExpectStatus(t, rec, http.StatusOK)

	// A key's import is compared with the key's own rules.
	rec, got := wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"scope":"key","key_id":"key-1","rules":[{"pattern":"KEYRULE"},{"pattern":"other"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if reviews := reviewByIndex(t, got); reviews[0]["existing_id"] != "k-rule" || len(reviews) != 1 {
		t.Fatalf("reviews = %v", got["reviews"])
	}
	rec, _ = wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"scope":"key","key_id":"no-such-key","rules":[]}`, "")
	wordExpectStatus(t, rec, http.StatusNotFound)
	rec, _ = wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"scope":"key","rules":[]}`, "")
	wordExpectStatus(t, rec, http.StatusBadRequest)
	rec, _ = wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"scope":"galaxy","rules":[]}`, "")
	wordExpectStatus(t, rec, http.StatusBadRequest)
	rec, _ = wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{`, "")
	wordExpectStatus(t, rec, http.StatusBadRequest)

	// The platform policy can only be imported into by those who may edit it.
	rec, _ = wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"scope":"platform","rules":[{"pattern":"x"}]}`, "")
	wordExpectStatus(t, rec, http.StatusForbidden)
	rec, _ = wordPlatformAdmin.call(h.PostWordPolicyValidate, http.MethodPost, `{"scope":"platform","rules":[{"pattern":"x"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
}

func createPolicyKeyFor(t *testing.T, tenantID string) {
	t.Helper()
	createWordPolicyKey(t, tenantID, "key-1", "The key")
}

func TestWordPolicyTestShowsTheReplacedTextAndWhereTheHitsAre(t *testing.T) {
	h := setupWordPolicyTest(t)
	rec, _ := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut,
		`{"action":"replace","replacement":"[x]","reply_filter":true,"rules":[{"pattern":"badword"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	// The emoji takes two UTF-16 code units, so the offsets are those a
	// JavaScript string index would give; the second hit matched only after the
	// full-width letters were folded.
	rec, got := wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"😀 ok BADWORD and ｂａｄｗｏｒｄ"}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got["action"] != "replace" || got["blocked"] != false || got["output"] != "😀 ok [x] and [x]" || got["replaced"] != float64(2) || got["match_count"] != float64(2) {
		t.Fatalf("result = %v", got)
	}
	matches := got["matches"].([]any)
	first, second := matches[0].(map[string]any), matches[1].(map[string]any)
	if first["start"] != float64(6) || first["end"] != float64(13) || first["normalized_only"] != true || first["layer"] != "tenant" || first["kind"] != "keyword" {
		t.Fatalf("first hit = %v", first)
	}
	if second["start"] != float64(18) || second["end"] != float64(25) || second["normalized_only"] != true {
		t.Fatalf("second hit = %v", second)
	}
	if got["reply_filter"] != true || got["reply_output"] != "😀 ok [x] and [x]" {
		t.Fatalf("reply preview = %v / %v", got["reply_filter"], got["reply_output"])
	}

	// A hit written exactly as listed is not flagged as normalization-only.
	_, got = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"a badword b"}`, "")
	if hit := got["matches"].([]any)[0].(map[string]any); hit["normalized_only"] != false || hit["start"] != float64(2) {
		t.Fatalf("hit = %v", hit)
	}

	// Testing stores nothing.
	_, stored := wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
	if stored["version"] != float64(1) {
		t.Fatalf("the test changed the policy: %v", stored)
	}
}

func TestWordPolicyTestLayersPlatformTenantAndKey(t *testing.T) {
	h := setupWordPolicyTest(t)
	createPolicyKeyFor(t, wordTenantA)
	rec, _ := wordPlatformAdmin.call(h.PutWordPolicyPlatform, http.MethodPut, `{"rules":[{"pattern":"platformword"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	rec, _ = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"action":"replace","rules":[{"pattern":"tenantword"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	rec, _ = wordTenantAUser.call(h.PutWordPolicyKey, http.MethodPut, `{"action":"log","rules":[{"pattern":"keyword"}]}`, "key-1")
	wordExpectStatus(t, rec, http.StatusOK)

	// The platform's block cannot be softened by anything below it.
	rec, got := wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"has platformword and tenantword","key_id":"key-1"}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got["blocked"] != true || got["action"] != "block" || got["output"] != "has platformword and tenantword" || got["block_message"] == "" {
		t.Fatalf("result = %v", got)
	}
	if layers := got["layers"].([]any); len(layers) != 2 || layers[0] != "platform" || layers[1] != "key" {
		t.Fatalf("layers = %v", layers)
	}

	// Without the key, the tenant's replace applies; with it, the key's log does.
	_, got = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"a tenantword b"}`, "")
	if got["action"] != "replace" || got["output"] != "a *** b" {
		t.Fatalf("tenant only: %v", got)
	}
	if layers := got["layers"].([]any); len(layers) != 2 || layers[1] != "tenant" {
		t.Fatalf("layers = %v", layers)
	}
	_, got = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"a tenantword keyword b","key_id":"key-1"}`, "")
	if got["action"] != "log" || got["output"] != "a tenantword keyword b" || got["match_count"] != float64(2) {
		t.Fatalf("with the key: %v", got)
	}

	// A tenant with no policy of its own is still held to the platform's.
	_, got = wordTenantBUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"platformword"}`, "")
	if got["blocked"] != true {
		t.Fatalf("tenant B escapes the platform policy: %v", got)
	}
}

func TestWordPolicyTestWithADraft(t *testing.T) {
	h := setupWordPolicyTest(t)
	createPolicyKeyFor(t, wordTenantA)
	rec, _ := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[{"pattern":"stored"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	// The draft stands in for the stored policy; the stored one is not consulted.
	rec, got := wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost,
		`{"input":"stored and drafted","draft":{"action":"replace","replacement":"#","rules":[{"pattern":"drafted"}]}}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got["action"] != "replace" || got["output"] != "stored and #" {
		t.Fatalf("draft result = %v", got)
	}
	_, stored := wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
	if stored["version"] != float64(1) || wordRuleList(t, stored)[0]["pattern"] != "stored" {
		t.Fatalf("the draft was stored: %v", stored)
	}

	// A draft key policy needs its key.
	rec, got = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost,
		`{"input":"x draftkey","scope":"key","key_id":"key-1","draft":{"action":"replace","rules":[{"pattern":"draftkey"}]}}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got["output"] != "x ***" {
		t.Fatalf("key draft = %v", got)
	}
	rec, _ = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","scope":"key","draft":{}}`, "")
	wordExpectStatus(t, rec, http.StatusBadRequest)

	// A broken draft is refused naming the rule, like a save.
	rec, got = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","draft":{"rules":[{"kind":"regex","pattern":"("}]}}`, "")
	wordExpectStatus(t, rec, http.StatusUnprocessableEntity)
	if problems := got["error"].(map[string]any)["rules"].([]any); len(problems) != 1 || problems[0].(map[string]any)["pattern"] != "(" {
		t.Fatalf("body = %v", got)
	}

	// Nobody but those who can edit the platform policy can stand in for it.
	rec, _ = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","scope":"platform","draft":{"rules":[{"pattern":"x"}]}}`, "")
	wordExpectStatus(t, rec, http.StatusForbidden)
	rec, got = wordPlatformAdmin.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","scope":"platform","draft":{"rules":[{"pattern":"x"}]}}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got["blocked"] != true {
		t.Fatalf("platform draft = %v", got)
	}
	rec, _ = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","scope":"galaxy","draft":{}}`, "")
	wordExpectStatus(t, rec, http.StatusBadRequest)
}

func TestWordPolicyTestInput(t *testing.T) {
	h := setupWordPolicyTest(t)

	rec, got := wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"anything"}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got["action"] != "allow" || got["output"] != "anything" || got["reply_filter"] != false {
		t.Fatalf("no policies: %v", got)
	}
	if matches, ok := got["matches"].([]any); !ok || len(matches) != 0 {
		t.Fatalf("matches must be an empty list: %v", got["matches"])
	}
	if layers, ok := got["layers"].([]any); !ok || len(layers) != 0 {
		t.Fatalf("layers must be an empty list: %v", got["layers"])
	}

	for name, payload := range map[string]string{
		"missing":    `{}`,
		"blank":      `{"input":"   "}`,
		"too long":   `{"input":"` + strings.Repeat("a", maxWordTestInputBytes+1) + `"}`,
		"not a body": ``,
	} {
		rec, _ = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, payload, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	rec, _ = wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","key_id":"missing"}`, "")
	wordExpectStatus(t, rec, http.StatusNotFound)
}
