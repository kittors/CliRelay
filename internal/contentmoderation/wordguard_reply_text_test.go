package contentmoderation

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// replyContexts builds the contexts a reply is filtered with: a tenant policy
// that has reply filtering on.
func replyContexts(t *testing.T, action string, rules ...WordRule) []wordContext {
	t.Helper()
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action = action
		p.ReplyFilter = boolPtr(true)
	}, rules...)
	return buildWordContexts(nil, tenant, nil)
}

func replyEngineOf(t *testing.T, action string, rules ...WordRule) (*replyEngine, *WordGuardSnapshot) {
	t.Helper()
	snapshot := newWordGuardSnapshot(WordVerdict{Action: WordVerdictAllow}, nil, "")
	engine := newReplyEngine(replyContexts(t, action, rules...), snapshot)
	if engine == nil {
		t.Fatal("no reply engine")
	}
	return engine, snapshot
}

// streamText filters pieces the way a stream does: what an event holds back is
// put in front of the next piece. The result is what the client reads.
func streamText(engine *replyEngine, pieces ...string) string {
	var out strings.Builder
	carry := ""
	for _, piece := range pieces {
		r := engine.filterText(carry+piece, true)
		out.WriteString(r.out)
		carry = r.held
	}
	out.WriteString(engine.filterText(carry, false).out)
	return out.String()
}

func TestReplyFilterTextReplacesAKeyword(t *testing.T) {
	engine, snapshot := replyEngineOf(t, WordActionReplace, kw("bad"))
	r := engine.filterText("a bad word, Bad too", false)
	if r.out != "a *** word, *** too" || r.held != "" {
		t.Fatalf("got %+v", r)
	}
	if view := snapshot.view.Reply; view == nil || view.Hits != 2 || view.Replaced != 2 {
		t.Fatalf("reply counters = %+v", view)
	}
}

func TestReplyFilterTextBlockActionReplacesBecauseThereIsNobodyToRefuse(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionBlock, kw("bad"))
	if got := engine.filterText("so bad", false).out; got != "so ***" {
		t.Fatalf("got %q", got)
	}
}

func TestReplyFilterTextLogActionCountsAndLeavesTheText(t *testing.T) {
	engine, snapshot := replyEngineOf(t, WordActionLog, kw("bad"))
	r := engine.filterText("so bad, very bad", true)
	if r.out != "so bad, very bad" || r.held != "" {
		t.Fatalf("a log-only filter changed or held the text: %+v", r)
	}
	if view := snapshot.view.Reply; view == nil || view.Hits != 2 || view.Replaced != 0 {
		t.Fatalf("reply counters = %+v", view)
	}
}

func TestReplyFilterTextHoldsBackTheStartOfAKeyword(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, kw("badword"))
	r := engine.filterText("this is a bad", true)
	if r.out != "this is a " || r.held != "bad" {
		t.Fatalf("got %+v", r)
	}
	// What follows decides it.
	if got := engine.filterText(r.held+"word!", true); got.out != "***!" || got.held != "" {
		t.Fatalf("joined = %+v", got)
	}
	if got := engine.filterText(r.held+"ly", true); got.out != "badly" || got.held != "" {
		t.Fatalf("not a match = %+v", got)
	}
}

func TestReplyFilterTextHeldTextIsDecidedWhenTheEndIsFinal(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, kw("bad"), kw("badword"))
	// "bad" is a keyword, and "badword" could still follow.
	r := engine.filterText("so bad", true)
	if r.out != "so " || r.held != "bad" {
		t.Fatalf("got %+v", r)
	}
	if got := engine.filterText(r.held, false); got.out != "***" || got.held != "" {
		t.Fatalf("final = %+v", got)
	}
}

func TestReplyFilterTextKeepsTheOriginalSpellingOfWhatIsHeld(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, kw("badword"))
	// Full-width letters, a zero-width joiner and upper case are all normalized
	// away for matching, and must still be what is held.
	r := engine.filterText("x Ｂa\u200dD", true)
	if r.out != "x " || r.held != "Ｂa\u200dD" {
		t.Fatalf("got %+v", r)
	}
	if got := engine.filterText(r.held+"WORD", false); got.out != "***" {
		t.Fatalf("joined = %+v", got)
	}
}

func TestReplyFilterTextReplacesWhatMatchedAfterNormalizationOnly(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, kw("badword"))
	got := engine.filterText("a B\u200bAＤWORD b", false).out
	if got != "a *** b" {
		t.Fatalf("got %q", got)
	}
	// A run of white space is one space to the matcher, whatever it is made of.
	spaced, _ := replyEngineOf(t, WordActionReplace, kw("bad word"))
	if got := spaced.filterText("a bad \t\n word b", false).out; got != "a *** b" {
		t.Fatalf("got %q", got)
	}
}

func TestReplyFilterTextChineseKeywordSplitAcrossPieces(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, kw("敏感词"))
	if got := streamText(engine, "这是一个敏", "感", "词测试"); got != "这是一个***测试" {
		t.Fatalf("got %q", got)
	}
	if got := streamText(engine, "这是一个敏", "感", "字测试"); got != "这是一个敏感字测试" {
		t.Fatalf("a near miss changed: %q", got)
	}
}

func TestReplyFilterTextRegexHoldsAMatchThatRunsToTheEnd(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, rx(`\d{3,}`))
	r := engine.filterText("call 12345", true)
	if r.out != "call " || r.held != "12345" {
		t.Fatalf("got %+v", r)
	}
	if got := engine.filterText(r.held+"67 now", true); got.out != "*** now" {
		t.Fatalf("got %+v", got)
	}
}

// A match that never ends must not be held for ever.
func TestReplyFilterTextCutsAnEndlessMatchAtTheHoldLimit(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, rx(`\d+`))
	r := engine.filterText(strings.Repeat("7", maxReplyHold+10), true)
	if r.held != "" || r.out != "***" {
		t.Fatalf("held %d bytes, out %q", len(r.held), r.out)
	}
}

func TestReplyFilterTextTwoContextsKeepTheirOwnReplacementsAndNothingHalf(t *testing.T) {
	platform := policyOf(t, WordScopePlatform, "", func(p *WordPolicy) {
		p.Action, p.Replacement, p.ReplyFilter = WordActionReplace, "[P]", boolPtr(true)
	}, kw("alpha"))
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action, p.Replacement, p.ReplyFilter = WordActionReplace, "[T]", boolPtr(true)
	}, kw("betagamma"))
	engine := newReplyEngine(buildWordContexts(platform, tenant, nil), nil)
	// "beta" is held for the tenant's keyword; the platform's hit in front of it
	// is decided and replaced at once.
	r := engine.filterText("alpha and beta", true)
	if r.out != "[P] and " || r.held != "beta" {
		t.Fatalf("got %+v", r)
	}
	if got := streamText(engine, "alpha and beta", "gamma!"); got != "[P] and [T]!" {
		t.Fatalf("got %q", got)
	}
}

func TestReplyFilterTextReplyOptInIsPerContext(t *testing.T) {
	platform := policyOf(t, WordScopePlatform, "", func(p *WordPolicy) { p.Action = WordActionReplace }, kw("alpha"))
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action, p.ReplyFilter = WordActionReplace, boolPtr(true)
	}, kw("beta"))
	engine := newReplyEngine(buildWordContexts(platform, tenant, nil), nil)
	if got := engine.filterText("alpha beta", false).out; got != "alpha ***" {
		t.Fatalf("only the context that asked for it filters the reply: %q", got)
	}
	if newReplyEngine(buildWordContexts(platform, nil, nil), nil) != nil {
		t.Fatal("no context asked for reply filtering, yet there is an engine")
	}
}

func TestReplyFilterTextInvalidUTF8IsNotRewritten(t *testing.T) {
	engine, snapshot := replyEngineOf(t, WordActionReplace, kw("bad"))
	text := "bad \xff bad"
	if got := engine.filterText(text, false).out; got != text {
		t.Fatalf("a text that cannot be traced back was rewritten: %q", got)
	}
	if view := snapshot.view.Reply; view == nil || view.Skipped != "untraceable_text" {
		t.Fatalf("the skip was not reported: %+v", view)
	}
}

// The central property of the stream filter: however the reply is cut into
// events, the client reads the same text as if it had been filtered whole.
func TestReplyFilterTextStreamingEqualsWholeText(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	alphabet := []string{"a", "b", "c", "ab", "bc", "x", " ", "  ", "\n", "A", "B", "ａ", "\u200b", "敏", "感", "词", "，", "é", "Ⅻ"}
	keywords := []string{"ab", "abc", "bca", "abcab", "aab", "敏感", "敏感词", "xx a", "b c", "ⅫⅫ", "cab x"}
	for trial := 0; trial < 400; trial++ {
		var rules []WordRule
		for _, i := range rng.Perm(len(keywords))[:2+rng.IntN(4)] {
			rules = append(rules, kw(keywords[i]))
		}
		engine, _ := replyEngineOf(t, WordActionReplace, rules...)
		var whole strings.Builder
		for n := 3 + rng.IntN(40); n > 0; n-- {
			whole.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := whole.String()
		want := engine.filterText(text, false).out

		// Cut the text at random byte offsets that fall between characters.
		var pieces []string
		for from := 0; from < len(text); {
			to := from + 1 + rng.IntN(7)
			for to < len(text) && !isRuneStart(text[to]) {
				to++
			}
			if to > len(text) {
				to = len(text)
			}
			pieces = append(pieces, text[from:to])
			from = to
		}
		if got := streamText(engine, pieces...); got != want {
			t.Fatalf("rules %v\ntext   %q\npieces %q\nwhole  %q\nstream %q", rules, text, pieces, want, got)
		}
	}
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
