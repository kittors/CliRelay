package privacymask

import (
	"reflect"
	"slices"
	"testing"
)

// A rule run only over the lines that can hold a match finds what it finds
// run over the whole text: every check after the pattern reads the whole
// text, and no built-in pattern but the private key's crosses a line.
func TestScanFindsWhatTheWholeTextHas(t *testing.T) {
	corpus := benchHistory(64 << 10)
	for _, c := range evalCases {
		corpus = append(corpus, c.Text)
	}
	corpus = append(corpus,
		"a\nOPENAI_API_KEY="+testOpenAIKey+"\n\nDB_PASSWORD=S3cr3t!x9\n  key: Zq8vT2xLk9RbW4nZpE6s\n",
		"ssh deploy@build-01.acme.cn\nuser@host:~$ ls\n13800138000\n4111 1111 1111 1111\n00:1a:2b:3c:4d:5e\n10.0.0.1 and 8.8.8.8\nGB82WEST12345698765432",
		"line one\r\nAuthorization: Bearer 9f8e7d6c5b4a39281706f5e4d3c2b1a0\r\nCookie: sid=abc123def456ghi\r\n",
	)
	all := Options{Secrets: true, Personal: true, Kinds: map[string]bool{"home": true, "userhost": true, "ip": true, "bucket": true}}
	for _, s := range corpus {
		resetRuleMemo()
		got := ruleSpans(s, all)
		want := wholeTextSpans(s, all)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("line scan differs on %q:\n got %+v\nwant %+v", s[:min(120, len(s))], got, want)
		}
	}
}

// wholeTextSpans is ruleSpans as it was before scan.go: every rule over the
// whole text.
func wholeTextSpans(s string, o Options) []span {
	var found, split []span
	lower := lowerASCII(s)
	for _, r := range rules {
		if !o.covers(r.cat) || len(r.markers) > 0 && !containsAny(s, r.markers) || len(r.fold) > 0 && !containsAny(lower, r.fold) {
			continue
		}
		for _, m := range r.matches(s, lower) {
			if len(r.groups) > 0 {
				took := false
				for g, kind := range r.groups {
					if a, b := m[2*g+2], m[2*g+3]; a >= 0 && r.takes(s, a, b, r.groupOK[g]) {
						found = append(found, span{a, b, kind, r.by(), 0, false})
						took = true
					}
				}
				if took {
					split = append(split, span{m[2], m[1], "", "", 0, false})
				}
				continue
			}
			a, b := m[0], m[1]
			for g := 1; 2*g+1 < len(m); g++ {
				if m[2*g] >= 0 {
					a, b = m[2*g], m[2*g+1]
					break
				}
				if !r.anyGroup {
					break
				}
			}
			ok := r.ok
			if r.fix != nil {
				var good bool
				if a, b, good = r.fix(s, a, b); !good {
					continue
				}
				ok = nil
			}
			if r.takes(s, a, b, ok) {
				found = append(found, span{a, b, r.kind, r.by(), 0, false})
			}
		}
	}
	for _, sp := range split {
		found = slices.DeleteFunc(found, func(f span) bool {
			return f.kind == "EMAIL" && f.start == sp.start && f.end <= sp.end
		})
	}
	return found
}
