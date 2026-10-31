package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

// jsonEscapeAll writes every character of s as a \uXXXX escape, the way
// Python's json.dumps does for anything that is not ASCII, and the longest way
// a JSON string can spell a character.
func jsonEscapeAll(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x10000 {
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			continue
		}
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

func TestWordRuleListIsCountedWhileItIsRead(t *testing.T) {
	// Three bytes on the wire, more than a hundred in memory once decoded: the
	// amplification a review measured at 600 MB for a body at the old limit.
	const elements = 2_000_000
	data := []byte("[" + strings.Repeat("{},", elements) + "{}]")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var rules wordRuleInputs
	err := json.Unmarshal(data, &rules)
	runtime.ReadMemStats(&after)

	var tooMany *tooManyWordRulesError
	if !errors.As(err, &tooMany) || tooMany.limit != contentmoderation.MaxWordRulesPerPolicy {
		t.Fatalf("err = %v, want too many rules", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("refusing %d rules allocated %d MB: they were decoded before they were counted", elements, allocated>>20)
	}
	if len(rules) != 0 {
		t.Fatalf("a refused list left %d rules behind", len(rules))
	}
}

func TestWordRuleListDecodesLikeAnyList(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want int
		err  bool
	}{
		"empty":       {`[]`, 0, false},
		"null":        {`null`, 0, false},
		"two":         {`[{"pattern":"a"},{"pattern":"b","enabled":false}]`, 2, false},
		"exactly max": {"[" + strings.Repeat(`{"pattern":"a"},`, contentmoderation.MaxWordRulesPerPolicy-1) + `{"pattern":"a"}]`, contentmoderation.MaxWordRulesPerPolicy, false},
		"not a list":  {`{"pattern":"a"}`, 0, true},
		"a string":    {`"a"`, 0, true},
		"bad rule":    {`[{"pattern":5}]`, 0, true},
		"truncated":   {`[{"pattern":"a"},`, 0, true},
	} {
		var rules wordRuleInputs
		err := json.Unmarshal([]byte(tc.in), &rules)
		if (err != nil) != tc.err || len(rules) != tc.want {
			t.Errorf("%s: %d rules, err = %v", name, len(rules), err)
		}
	}
	var in wordPolicyInput
	if err := json.Unmarshal([]byte(`{"rules":[{"pattern":"a","enabled":false}],"version":3}`), &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Rules) != 1 || in.Rules[0].Enabled == nil || *in.Rules[0].Enabled || in.Version == nil || *in.Version != 3 {
		t.Fatalf("decoded = %+v", in)
	}
}

func TestWordPolicyTooManyRulesIsAnsweredAsTheSaveWouldAnswer(t *testing.T) {
	h := setupWordPolicyTest(t)
	over := "[" + strings.Repeat(`{"pattern":"a"},`, contentmoderation.MaxWordRulesPerPolicy) + `{"pattern":"a"}]`

	requests := map[string]struct {
		handler func() (int, map[string]any)
	}{
		"save": {func() (int, map[string]any) {
			rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":`+over+`}`, "")
			return rec.Code, body
		}},
		"validate": {func() (int, map[string]any) {
			rec, body := wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"rules":`+over+`}`, "")
			return rec.Code, body
		}},
		"test draft": {func() (int, map[string]any) {
			rec, body := wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","draft":{"rules":`+over+`}}`, "")
			return rec.Code, body
		}},
	}
	for name, request := range requests {
		status, body := request.handler()
		if status != http.StatusUnprocessableEntity || wordErrorCode(body) != "word_policy_invalid" {
			t.Errorf("%s: status %d, body %v", name, status, body)
			continue
		}
		problems, _ := body["error"].(map[string]any)["rules"].([]any)
		if len(problems) != 1 {
			t.Errorf("%s: problems = %v", name, problems)
			continue
		}
		problem := problems[0].(map[string]any)
		if problem["code"] != "too_many_rules" || problem["index"] != float64(contentmoderation.MaxWordRulesPerPolicy) {
			t.Errorf("%s: problem = %v", name, problem)
		}
	}
}

func TestWordPolicyHoldsExactlyTheMaximumNumberOfRules(t *testing.T) {
	h := setupWordPolicyTest(t)
	var rules []string
	for i := 0; i < contentmoderation.MaxWordRulesPerPolicy; i++ {
		rules = append(rules, `{"pattern":"word`+strconv.Itoa(i)+`"}`)
	}
	rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[`+strings.Join(rules, ",")+`]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got := len(wordRuleList(t, body)); got != contentmoderation.MaxWordRulesPerPolicy {
		t.Fatalf("saved %d rules", got)
	}
}

// worstKeywordRule and worstRegexRule are the largest rules a client can send,
// written the longest way: every character an escape, long names for the id and
// the note, and the indentation of a pretty-printer.
func worstKeywordRule(i int) string {
	pattern := make([]rune, contentmoderation.MaxWordKeywordRunes)
	for j := range pattern {
		// CJK extension B: visible, outside the Basic Multilingual Plane, and
		// different from the next rule's by the place it starts.
		pattern[j] = rune(0x20000 + i%30000 + j)
	}
	note := strings.Repeat(string(rune(0x1F600)), contentmoderation.MaxWordNoteRunes)
	id := fmt.Sprintf("%064d", i)
	return "    {\n" +
		`      "id": "` + jsonEscapeAll(id) + "\",\n" +
		`      "kind": "keyword",` + "\n" +
		`      "pattern": "` + jsonEscapeAll(string(pattern)) + "\",\n" +
		`      "enabled": false,` + "\n" +
		`      "ignore_separators": false,` + "\n" +
		`      "note": "` + jsonEscapeAll(note) + "\"\n" +
		"    }"
}

func worstRegexRule(i int) string {
	var pattern strings.Builder
	pattern.WriteString("[")
	for pattern.Len() <= contentmoderation.MaxWordRegexBytes-9 {
		fmt.Fprintf(&pattern, `\x{%04x}`, 0x4e00+i*100+pattern.Len()/8) // one class: a single instruction
	}
	pattern.WriteString("]")
	if pattern.Len() > contentmoderation.MaxWordRegexBytes {
		panic("regex source over the limit")
	}
	note := strings.Repeat(string(rune(0x1F600)), contentmoderation.MaxWordNoteRunes)
	return "    {\n" +
		`      "id": "` + jsonEscapeAll(fmt.Sprintf("%064d", 100000+i)) + "\",\n" +
		`      "kind": "regex",` + "\n" +
		`      "pattern": "` + jsonEscapeAll(pattern.String()) + "\",\n" +
		`      "enabled": false,` + "\n" +
		`      "note": "` + jsonEscapeAll(note) + "\"\n" +
		"    }"
}

func worstPolicyBody() string {
	var rules []string
	for i := 0; i < contentmoderation.MaxWordRulesPerPolicy-contentmoderation.MaxWordRegexPerPolicy; i++ {
		rules = append(rules, worstKeywordRule(i))
	}
	for i := 0; i < contentmoderation.MaxWordRegexPerPolicy; i++ {
		rules = append(rules, worstRegexRule(i))
	}
	return "{\n  \"action\": \"log\",\n  \"replacement\": \"" + jsonEscapeAll(strings.Repeat(string(rune(0x1F600)), contentmoderation.MaxWordReplacementRunes)) + "\",\n" +
		"  \"block_message\": \"" + jsonEscapeAll(strings.Repeat(string(rune(0x1F600)), contentmoderation.MaxWordBlockMessageRunes)) + "\",\n" +
		"  \"rules\": [\n" + strings.Join(rules, ",\n") + "\n  ]\n}"
}

func TestWordPolicyBodyLimitHoldsTheLargestLegalPolicy(t *testing.T) {
	if len(worstKeywordRule(0)) > maxKeywordRuleJSONBytes || len(worstRegexRule(0)) > maxRegexRuleJSONBytes {
		t.Fatalf("a rule is %d (keyword) / %d (regex) bytes, over the %d / %d allowed for", len(worstKeywordRule(0)), len(worstRegexRule(0)), maxKeywordRuleJSONBytes, maxRegexRuleJSONBytes)
	}
	body := worstPolicyBody()
	if len(body) > maxWordPolicyBodyBytes {
		t.Fatalf("the largest legal policy is %d bytes and the body limit %d: a legal policy would be refused", len(body), maxWordPolicyBodyBytes)
	}
	// The limit is a bound on what can be legal, not a round number: it has to
	// stay close to the largest policy there is.
	if len(body) < maxWordPolicyBodyBytes*9/10 {
		t.Fatalf("the limit %d is far above the largest legal policy, %d bytes", maxWordPolicyBodyBytes, len(body))
	}
}

func TestWordPolicySavesTheLargestLegalPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 44 MiB request")
	}
	h := setupWordPolicyTest(t)
	rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, worstPolicyBody(), "")
	wordExpectStatus(t, rec, http.StatusOK)
	if got := len(wordRuleList(t, body)); got != contentmoderation.MaxWordRulesPerPolicy {
		t.Fatalf("saved %d rules", got)
	}
}

func TestWordPolicyBodyJustOverTheLimitIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 44 MiB request")
	}
	h := setupWordPolicyTest(t)
	padding := strings.Repeat(" ", maxWordPolicyBodyBytes)
	rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[]`+padding+`}`, "")
	wordExpectStatus(t, rec, http.StatusRequestEntityTooLarge)
	if wordErrorCode(body) != "word_policy_too_large" {
		t.Fatalf("body = %v", body)
	}
}

func TestWordPolicyTestAcceptsTheLargestInputWrittenAsEscapes(t *testing.T) {
	h := setupWordPolicyTest(t)
	input := strings.Repeat("a", maxWordTestInputBytes)
	rec, _ := wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"`+jsonEscapeAll(input)+`"}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
}

func TestWordValidateCountsTheCompiledSizeOfRegularExpressions(t *testing.T) {
	h := setupWordPolicyTest(t)
	rec, _ := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut,
		`{"rules":[{"kind":"regex","pattern":"a{400}"},{"kind":"regex","pattern":"b{400}"},{"kind":"regex","pattern":"c{400}"},{"kind":"regex","pattern":"d{400}"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	validate := func(rule string) map[string]any {
		t.Helper()
		rec, body := wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"rules":[`+rule+`]}`, "")
		wordExpectStatus(t, rec, http.StatusOK)
		return body["summary"].(map[string]any)
	}
	summary := validate(`{"kind":"regex","pattern":"e{400}"}`)
	if summary["existing_regex_program"] != float64(4*402) || summary["new_regex_program"] != float64(402) || summary["fits"] != false {
		t.Fatalf("a fifth large expression does not fit in the policy: %v", summary)
	}
	summary = validate(`{"kind":"regex","pattern":"e{100}"}`)
	if summary["new_regex_program"] != float64(102) || summary["fits"] != true {
		t.Fatalf("a small one does: %v", summary)
	}

	rec, body := wordTenantAUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"rules":[{"kind":"regex","pattern":"(?:a?){1000}a{1000}"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	reviews, _ := body["reviews"].([]any)
	if len(reviews) != 1 || reviews[0].(map[string]any)["problem"].(map[string]any)["code"] != "regex_too_complex" {
		t.Fatalf("reviews = %v", reviews)
	}
}

func TestWordPolicySaveRefusesAnOverlyComplexExpression(t *testing.T) {
	h := setupWordPolicyTest(t)
	rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[{"kind":"regex","pattern":"(?:a?){1000}a{1000}"}]}`, "")
	wordExpectStatus(t, rec, http.StatusUnprocessableEntity)
	problems, _ := body["error"].(map[string]any)["rules"].([]any)
	if len(problems) != 1 || problems[0].(map[string]any)["code"] != "regex_too_complex" {
		t.Fatalf("body = %v", body)
	}
}

func TestWordPolicyRuleIDIsBounded(t *testing.T) {
	h := setupWordPolicyTest(t)
	rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut,
		`{"rules":[{"id":"`+strings.Repeat("i", contentmoderation.MaxWordRuleIDBytes+1)+`","pattern":"a"}]}`, "")
	wordExpectStatus(t, rec, http.StatusUnprocessableEntity)
	problems, _ := body["error"].(map[string]any)["rules"].([]any)
	if len(problems) != 1 || problems[0].(map[string]any)["code"] != "too_long" {
		t.Fatalf("body = %v", body)
	}
}
