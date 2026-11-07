package payloadrules

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

// Section names a list of rules in config.PayloadConfig, as the YAML does.
type Section string

const (
	SectionDefault     Section = "default"
	SectionDefaultRaw  Section = "default-raw"
	SectionOverride    Section = "override"
	SectionOverrideRaw Section = "override-raw"
	SectionFilter      Section = "filter"
)

// What a param did.
const (
	ActionSet     = "set"
	ActionRemoved = "removed"
	ActionSkipped = "skipped"
)

// Why a param was skipped.
const (
	// A default rule leaves a field the client sent.
	SkipPresent = "present"
	// Defaults are first write wins: an earlier default rule wrote the field.
	SkipAlreadyDefaulted = "already-defaulted"
	// A filter path the body does not have.
	SkipAbsent = "absent"
	// The path is empty or cannot be written.
	SkipInvalidPath = "invalid-path"
	// A *-raw rule's value is missing.
	SkipInvalidValue = "invalid-value"
)

// Why a rule did not match.
const (
	// The rule lists no model.
	MissNoModels = "no-models"
	// A model entry matches the model, but it is restricted to another protocol.
	MissProtocol = "protocol"
	// No model entry matches the model.
	MissModel = "model"
)

// Result is what Explain found.
type Result struct {
	// Body is the rewritten body, byte for byte what Apply returns.
	Body []byte
	// Models are the model names the rules were matched against, in the order
	// they were tried.
	Models []string
	// Rules has one entry per rule, in the order the rules ran.
	Rules []RuleOutcome
	// Changes lists every path whose value differs between the input body and
	// Body.
	Changes []Change
}

// RuleOutcome is what one rule did.
type RuleOutcome struct {
	Section Section `json:"section"`
	// Index is the rule's position in its section, from 0.
	Index   int  `json:"index"`
	Matched bool `json:"matched"`
	// Pattern is the model entry that matched, Model the name it matched.
	Pattern string `json:"pattern,omitempty"`
	Model   string `json:"model,omitempty"`
	// Miss says why the rule did not match.
	Miss string `json:"miss,omitempty"`
	// Params says what each param of a matched rule did, in the order they ran.
	Params []ParamOutcome `json:"params,omitempty"`
}

// ParamOutcome is what one param of a matched rule did.
type ParamOutcome struct {
	// Path is the path the param addressed, under the request's root.
	Path   string `json:"path"`
	Action string `json:"action"`
	// Reason says why the param was skipped.
	Reason string `json:"reason,omitempty"`
	// Value is the JSON the param wrote.
	Value string `json:"value,omitempty"`
}

// Change is one path whose value the rules changed.
type Change struct {
	// Path is in gjson syntax, with '.', '*' and '?' in keys escaped.
	Path string `json:"path"`
	// Before and After are the JSON at Path, nil where the path is absent.
	Before *string `json:"before,omitempty"`
	After  *string `json:"after,omitempty"`
}

// Explain applies rules like Apply and records what each rule did.
func Explain(rules config.PayloadConfig, req Request) Result {
	trace := &tracer{}
	body := run(rules, req, trace)
	return Result{
		Body:    body,
		Models:  trace.models,
		Rules:   trace.rules,
		Changes: diffJSON(req.Body, body),
	}
}

// tracer records what run does. A nil tracer records nothing: methods called
// on the request path only compute what run needs.
type tracer struct {
	models []string
	rules  []RuleOutcome
}

func (t *tracer) setModels(models []string) {
	if t != nil {
		t.models = append([]string(nil), models...)
	}
}

// rule reports whether the rule matches and records the outcome.
func (t *tracer) rule(section Section, index int, entries []config.PayloadModelRule, protocol string, candidates []string) bool {
	pattern, model, ok := matchedEntry(entries, protocol, candidates)
	if t == nil {
		return ok
	}
	outcome := RuleOutcome{Section: section, Index: index, Matched: ok, Pattern: pattern, Model: model}
	if !ok {
		outcome.Miss = missReason(entries, candidates)
	}
	t.rules = append(t.rules, outcome)
	return ok
}

// missReason tells an entry restricted to another protocol apart from no entry
// matching the model at all; matchedEntry already found that none matches with
// the protocol.
func missReason(entries []config.PayloadModelRule, candidates []string) string {
	named := false
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		named = true
		for _, candidate := range candidates {
			if MatchModelPattern(name, candidate) {
				return MissProtocol
			}
		}
	}
	if !named {
		return MissNoModels
	}
	return MissModel
}

func (t *tracer) param(outcome ParamOutcome) {
	if t == nil || len(t.rules) == 0 {
		return
	}
	last := &t.rules[len(t.rules)-1]
	last.Params = append(last.Params, outcome)
}

// set records a param written into body.
func (t *tracer) set(path string, body []byte, value any, raw bool) {
	if t == nil {
		return
	}
	// Read the value back: that is exactly what was written. Paths gjson cannot
	// read, such as sjson's "-1" for appending, fall back to the rule's value.
	written := gjson.GetBytes(body, path).Raw
	if written == "" {
		if raw {
			rawValue, _ := rawParamValue(value)
			written = string(rawValue)
		} else {
			written = valueJSON(value)
		}
	}
	t.param(ParamOutcome{Path: path, Action: ActionSet, Value: written})
}

func (t *tracer) removed(path string) {
	t.param(ParamOutcome{Path: path, Action: ActionRemoved})
}

func (t *tracer) skip(path, reason string) {
	t.param(ParamOutcome{Path: path, Action: ActionSkipped, Reason: reason})
}

// valueJSON is a param value as JSON.
func valueJSON(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// diffJSON lists the paths whose values differ between two JSON documents.
// Objects are compared key by key and arrays index by index; any other value
// is reported whole where its text differs.
func diffJSON(before, after []byte) []Change {
	var changes []Change
	diffValue("", gjson.ParseBytes(before), gjson.ParseBytes(after), &changes)
	return changes
}

func diffValue(path string, a, b gjson.Result, changes *[]Change) {
	switch {
	case !a.Exists() && !b.Exists():
		return
	case a.IsObject() && b.IsObject():
		aFields := objectFields(a)
		seen := make(map[string]struct{}, len(aFields))
		b.ForEach(func(key, value gjson.Result) bool {
			name := key.String()
			if _, repeated := seen[name]; repeated {
				return true
			}
			seen[name] = struct{}{}
			diffValue(childPath(path, escapePathKey(name)), aFields[name], value, changes)
			return true
		})
		a.ForEach(func(key, value gjson.Result) bool {
			name := key.String()
			if _, ok := seen[name]; !ok {
				diffValue(childPath(path, escapePathKey(name)), value, gjson.Result{}, changes)
			}
			return true
		})
		return
	case a.IsArray() && b.IsArray():
		as, bs := a.Array(), b.Array()
		for i := 0; i < len(as) || i < len(bs); i++ {
			var x, y gjson.Result
			if i < len(as) {
				x = as[i]
			}
			if i < len(bs) {
				y = bs[i]
			}
			diffValue(childPath(path, strconv.Itoa(i)), x, y, changes)
		}
		return
	case a.Exists() && b.Exists() && a.Type == b.Type && a.Raw == b.Raw:
		return
	}
	*changes = append(*changes, Change{Path: path, Before: rawOf(a), After: rawOf(b)})
}

// objectFields indexes an object's fields by name; with a repeated key the
// first one counts, as gjson reads it.
func objectFields(object gjson.Result) map[string]gjson.Result {
	fields := make(map[string]gjson.Result)
	object.ForEach(func(key, value gjson.Result) bool {
		if _, ok := fields[key.String()]; !ok {
			fields[key.String()] = value
		}
		return true
	})
	return fields
}

func childPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// escapePathKey escapes '.', '*', '?' and '\' with a backslash, as gjson and
// sjson paths do, so a key with a dot in it still reads as one key.
func escapePathKey(key string) string {
	if !strings.ContainsAny(key, `.*?\`) {
		return key
	}
	var b strings.Builder
	for _, r := range key {
		switch r {
		case '.', '*', '?', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func rawOf(value gjson.Result) *string {
	if !value.Exists() {
		return nil
	}
	raw := value.Raw
	return &raw
}
