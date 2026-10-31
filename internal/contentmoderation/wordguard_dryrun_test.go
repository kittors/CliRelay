package contentmoderation

import "testing"

func TestDryRunReplacesAndLocatesHitsInUTF16(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action, p.Replacement, p.ReplyFilter = WordActionReplace, "[x]", boolPtr(true)
	}, kw("badword"))
	// An emoji is two UTF-16 code units, so the hit starts at 5, not at 3.
	text := "😀 ok BADWORD and ｂａｄｗｏｒｄ"
	result := DryRunWordPolicies(WordPolicies{Tenant: tenant}, text)
	if result.Action != WordVerdictReplace || result.Blocked || result.Output != "😀 ok [x] and [x]" || result.Replaced != 2 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Matches) != 2 {
		t.Fatalf("matches = %+v", result.Matches)
	}
	first, second := result.Matches[0], result.Matches[1]
	if first.Start != 6 || first.End != 13 || !first.NormalizedOnly || first.Layer != WordScopeTenant {
		t.Fatalf("first = %+v", first)
	}
	if second.Start != 18 || second.End != 25 || !second.NormalizedOnly {
		t.Fatalf("second = %+v", second)
	}
	if !result.ReplyFilter || result.ReplyOutput != "😀 ok [x] and [x]" {
		t.Fatalf("reply preview = %v %q", result.ReplyFilter, result.ReplyOutput)
	}
}

func TestDryRunReportsAMatchThatIsTheSameAsWritten(t *testing.T) {
	result := DryRunWordPolicies(WordPolicies{Tenant: tenantOf(t, WordActionLog, kw("bad"))}, "so bad")
	if result.Action != WordVerdictLog || result.Output != "so bad" || len(result.Matches) != 1 || result.Matches[0].NormalizedOnly {
		t.Fatalf("result = %+v", result)
	}
	if result.ReplyFilter || result.ReplyOutput != "" {
		t.Fatalf("reply filtering is off: %+v", result)
	}
}

func TestDryRunBlock(t *testing.T) {
	result := DryRunWordPolicies(WordPolicies{Platform: policyOf(t, WordScopePlatform, "", nil, kw("bad"))}, "so bad")
	if !result.Blocked || result.Action != WordVerdictBlock || result.Output != "so bad" || result.BlockMessage != DefaultBlockMessage {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Layers) != 1 || result.Layers[0] != WordScopePlatform {
		t.Fatalf("layers = %v", result.Layers)
	}
}

func TestDryRunWithoutPoliciesAllows(t *testing.T) {
	result := DryRunWordPolicies(WordPolicies{}, "anything")
	if result.Action != WordVerdictAllow || result.Output != "anything" || len(result.Matches) != 0 || len(result.Layers) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestReviewWordRules(t *testing.T) {
	existing := []WordRule{{ID: "r1", Kind: WordRuleKeyword, Pattern: "Alpha", Enabled: true}, {ID: "r2", Kind: WordRuleRegex, Pattern: `\d{4}`, Enabled: true}}
	incoming := []WordRule{
		kw("fresh"),                  // 0 new
		kw("  FRESH "),               // 1 same as 0 once trimmed and folded
		kw("alpha"),                  // 2 already stored (case only)
		rx(`\d{4}`),                  // 3 already stored
		rx(`(`),                      // 4 invalid
		kw(""),                       // 5 empty
		{Pattern: "no kind"},         // 6 kind defaults to keyword
		rx(`a*`),                     // 7 matches the empty string
		kw("alpha"),                  // 8 repeats an existing rule, which is not reported as a duplicate of 2
		{Kind: "glob", Pattern: "x"}, // 9 unknown kind
	}
	reviews := ReviewWordRules(incoming, existing)
	if len(reviews) != len(incoming) {
		t.Fatalf("%d reviews for %d rules", len(reviews), len(incoming))
	}
	want := []string{WordReviewNew, WordReviewDuplicate, WordReviewExisting, WordReviewExisting, WordReviewInvalid, WordReviewInvalid, WordReviewNew, WordReviewInvalid, WordReviewDuplicate, WordReviewInvalid}
	for i, review := range reviews {
		if review.Index != i || review.Status != want[i] {
			t.Errorf("rule %d: status %q, want %q (%+v)", i, review.Status, want[i], review)
		}
	}
	if reviews[1].DuplicateOf == nil || *reviews[1].DuplicateOf != 0 {
		t.Fatalf("duplicate of = %v", reviews[1].DuplicateOf)
	}
	if reviews[2].ExistingID != "r1" || reviews[3].ExistingID != "r2" {
		t.Fatalf("existing ids = %q %q", reviews[2].ExistingID, reviews[3].ExistingID)
	}
	if reviews[0].Rule == nil || reviews[0].Rule.Pattern != "fresh" || reviews[6].Rule == nil || reviews[6].Rule.Kind != WordRuleKeyword {
		t.Fatalf("stored forms = %+v / %+v", reviews[0].Rule, reviews[6].Rule)
	}
	for _, i := range []int{4, 5, 7, 9} {
		if reviews[i].Problem == nil || reviews[i].Problem.Index != i || reviews[i].Problem.Code == "" || reviews[i].Problem.Message == "" {
			t.Errorf("rule %d: no reason given: %+v", i, reviews[i])
		}
	}
	if reviews[4].Problem.Code != "invalid_regex" || reviews[7].Problem.Code != "matches_empty" || reviews[5].Problem.Code != "empty" {
		t.Errorf("problem codes = %q %q %q", reviews[4].Problem.Code, reviews[7].Problem.Code, reviews[5].Problem.Code)
	}
}
