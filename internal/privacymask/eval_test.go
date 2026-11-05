package privacymask

import (
	"regexp"
	"strings"
	"testing"
)

// The masking evaluation, ported from magpie (eval_cases_test.go): made-up
// texts of the kinds agents send, each with the values that must be masked and
// those that must go as written. magpie scores its rules alone and with its
// local model; there is no model here, so the rules are held to what magpie's
// rules do:
//   - every value a case keeps is kept, unless magpie marks it as one its own
//     masking still gets wrong (Known, GuardKnown);
//   - every value a case masks is masked, except those only a model finds
//     (names, addresses, an account named in prose), listed in modelOnly.

var testKey = []byte("0123456789abcdef0123456789abcdef")

// preview masks text as one request with o, remembering nothing.
func preview(text string, o Options) string {
	m := NewMasker(testKey, o, nil, "")
	return Apply(text, m.Plan([]string{text}, nil)[0])
}

var evalPlaceholders = regexp.MustCompile(`\{\{[A-Z_]+_[a-z2-7]{8}\}\}`)

// gone says v is not in out as it is: masked, or a part of it masked.
func gone(out, v string) bool {
	return !strings.Contains(evalPlaceholders.ReplaceAllString(out, "\x00"), v)
}

// modelOnly are the values magpie's cases mask that only its local model finds:
// what no rule can tell from the words around it.
var modelOnly = map[string]bool{"PERSON": true, "ADDRESS": true, "ACCOUNT": true}

// ruleMisses are values of a kind the rules find that the rules leave, each
// with why: magpie's rules leave them too, and its local model finds them.
var ruleMisses = map[string]string{
	"event-times/415 555 0132":       "a US number: the PHONE rules take mainland mobile numbers only",
	"us-address/555-0199":            "a US number",
	"phone-intl/415-555-0132":        "a US number",
	"tool-result-mixed/90-1234-5678": "a Japanese number",
	"tool-result-mixed/7700 900123":  "a UK number",
	"code-keys/" + evalGoogleKey:     "41 characters after AIza…: a Google key has 39, and the rule takes that form only",
}

func TestMaskEvalRules(t *testing.T) {
	o := Options{Secrets: true, Personal: true}
	masked, missed := 0, 0
	for _, c := range evalCases {
		out := preview(c.Text, o)
		for _, v := range c.Keep {
			if v.Known != "" || v.GuardKnown != "" {
				continue
			}
			if gone(out, v.V) {
				t.Errorf("%s: %q (%s) was masked, and should go as written\n%s", c.ID, v.V, v.Kind, out)
			}
		}
		for _, v := range c.Mask {
			if modelOnly[v.Kind] {
				continue
			}
			if gone(out, v.V) {
				masked++
				continue
			}
			missed++
			if why, ok := ruleMisses[c.ID+"/"+v.V]; ok {
				t.Logf("%s: %q left as written: %s", c.ID, v.V, why)
				continue
			}
			t.Errorf("%s: %s %q went as written\n%s", c.ID, v.Kind, v.V, out)
		}
	}
	t.Logf("rules masked %d values, left %d", masked, missed)
}
