// Package payloadrules applies the payload rules of the config
// (config.PayloadConfig) to an upstream request body.
//
// The executors run Apply on every request. The management dry-run runs
// Explain, which is the same code keeping a record of what each rule did, so
// what the panel shows for a sample body is what a request would get.
package payloadrules

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Request is one body the rules are applied to.
type Request struct {
	// Model is the upstream model the request resolved to.
	Model string
	// RequestedModel is the model the client asked for, before alias
	// resolution and with any thinking suffix ("my-alias(high)"), so a rule can
	// target an alias.
	RequestedModel string
	// Protocol is the format of Body ("openai", "claude", "codex", ...). A model
	// entry restricted to another protocol does not match; an empty Protocol
	// matches every entry.
	Protocol string
	// Root prefixes every rule path, for bodies that wrap the request: Gemini
	// CLI and Antigravity apply the rules inside "request".
	Root string
	// Body is the body to rewrite.
	Body []byte
	// Original is the client's request translated to the same format. Default
	// rules only write a field the client did not send, and in Body a field the
	// translator filled in looks the same as one the client sent. Empty means
	// Body.
	Original []byte
}

// Apply returns req.Body rewritten by rules.
func Apply(rules config.PayloadConfig, req Request) []byte {
	return run(rules, req, nil)
}

func hasRules(rules config.PayloadConfig) bool {
	return len(rules.Default) > 0 || len(rules.DefaultRaw) > 0 || len(rules.Override) > 0 ||
		len(rules.OverrideRaw) > 0 || len(rules.Filter) > 0
}

// run applies the rules in their fixed order: defaults (first write wins per
// field, checked against the original request), overrides (last write wins),
// then filters. trace is nil on the request path; Explain passes one.
func run(rules config.PayloadConfig, req Request, trace *tracer) []byte {
	payload := req.Body
	if len(payload) == 0 || !hasRules(rules) {
		return payload
	}
	candidates := modelCandidates(req.Model, req.RequestedModel)
	if len(candidates) == 0 {
		return payload
	}
	trace.setModels(candidates)
	out := payload
	source := req.Original
	if len(source) == 0 {
		source = payload
	}

	appliedDefaults := make(map[string]struct{})
	applyDefault := func(section Section, index int, rule *config.PayloadRule, raw bool) {
		if !trace.rule(section, index, rule.Models, req.Protocol, candidates) {
			return
		}
		for _, path := range sortedParamPaths(rule.Params) {
			value := rule.Params[path]
			fullPath := joinPath(req.Root, path)
			if fullPath == "" {
				trace.skip(path, SkipInvalidPath)
				continue
			}
			if gjson.GetBytes(source, fullPath).Exists() {
				trace.skip(fullPath, SkipPresent)
				continue
			}
			if _, ok := appliedDefaults[fullPath]; ok {
				trace.skip(fullPath, SkipAlreadyDefaulted)
				continue
			}
			updated, skipReason, ok := setParam(out, fullPath, value, raw)
			if !ok {
				trace.skip(fullPath, skipReason)
				continue
			}
			out = updated
			appliedDefaults[fullPath] = struct{}{}
			trace.set(fullPath, out, value, raw)
		}
	}
	for i := range rules.Default {
		applyDefault(SectionDefault, i, &rules.Default[i], false)
	}
	for i := range rules.DefaultRaw {
		applyDefault(SectionDefaultRaw, i, &rules.DefaultRaw[i], true)
	}

	applyOverride := func(section Section, index int, rule *config.PayloadRule, raw bool) {
		if !trace.rule(section, index, rule.Models, req.Protocol, candidates) {
			return
		}
		for _, path := range sortedParamPaths(rule.Params) {
			fullPath := joinPath(req.Root, path)
			if fullPath == "" {
				trace.skip(path, SkipInvalidPath)
				continue
			}
			value := rule.Params[path]
			updated, skipReason, ok := setParam(out, fullPath, value, raw)
			if !ok {
				trace.skip(fullPath, skipReason)
				continue
			}
			out = updated
			trace.set(fullPath, out, value, raw)
		}
	}
	for i := range rules.Override {
		applyOverride(SectionOverride, i, &rules.Override[i], false)
	}
	for i := range rules.OverrideRaw {
		applyOverride(SectionOverrideRaw, i, &rules.OverrideRaw[i], true)
	}

	for i := range rules.Filter {
		rule := &rules.Filter[i]
		if !trace.rule(SectionFilter, i, rule.Models, req.Protocol, candidates) {
			continue
		}
		for _, path := range rule.Params {
			fullPath := joinPath(req.Root, path)
			if fullPath == "" {
				trace.skip(path, SkipInvalidPath)
				continue
			}
			if trace != nil && !gjson.GetBytes(out, fullPath).Exists() {
				trace.skip(fullPath, SkipAbsent)
				continue
			}
			updated, err := sjson.DeleteBytes(out, fullPath)
			if err != nil {
				trace.skip(fullPath, SkipInvalidPath)
				continue
			}
			out = updated
			trace.removed(fullPath)
		}
	}
	return out
}

// setParam writes one param; when it cannot, skipReason says why.
func setParam(body []byte, path string, value any, raw bool) (updated []byte, skipReason string, ok bool) {
	if raw {
		rawValue, valid := rawParamValue(value)
		if !valid {
			return body, SkipInvalidValue, false
		}
		updated, err := sjson.SetRawBytes(body, path, rawValue)
		if err != nil {
			return body, SkipInvalidPath, false
		}
		return updated, "", true
	}
	updated, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body, SkipInvalidPath, false
	}
	return updated, "", true
}

// sortedParamPaths fixes the order a rule's params are written in. Params are a
// map, so without it two paths of one rule that overlap ("a" and "a.b") would
// give a different body from one request to the next.
func sortedParamPaths(params map[string]any) []string {
	paths := make([]string, 0, len(params))
	for path := range params {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// matchedEntry returns the first model entry that matches one of the
// candidates, trying the candidates in order.
func matchedEntry(entries []config.PayloadModelRule, protocol string, candidates []string) (pattern, model string, ok bool) {
	if len(entries) == 0 || len(candidates) == 0 {
		return "", "", false
	}
	for _, candidate := range candidates {
		for _, entry := range entries {
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				continue
			}
			if ep := strings.TrimSpace(entry.Protocol); ep != "" && protocol != "" && !strings.EqualFold(ep, protocol) {
				continue
			}
			if MatchModelPattern(name, candidate) {
				return name, candidate, true
			}
		}
	}
	return "", "", false
}

// modelCandidates lists the model names rules are matched against: the
// upstream model, then the requested model without and with its thinking
// suffix.
func modelCandidates(model, requestedModel string) []string {
	model = strings.TrimSpace(model)
	requestedModel = strings.TrimSpace(requestedModel)
	if model == "" && requestedModel == "" {
		return nil
	}
	candidates := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, value)
	}
	add(model)
	if requestedModel != "" {
		parsed := thinking.ParseSuffix(requestedModel)
		add(parsed.ModelName)
		if parsed.HasSuffix {
			add(requestedModel)
		}
	}
	return candidates
}

// joinPath puts a rule path under root. Without a root the path is used as is.
func joinPath(root, path string) string {
	r := strings.TrimSpace(root)
	p := strings.TrimSpace(path)
	if r == "" {
		return p
	}
	if p == "" {
		return r
	}
	return r + "." + strings.TrimPrefix(p, ".")
}

// rawParamValue is the JSON a *-raw rule writes. Strings are taken as JSON
// text; config loading drops rules whose text is not valid JSON.
func rawParamValue(value any) ([]byte, bool) {
	if value == nil {
		return nil, false
	}
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return nil, false
		}
		return raw, true
	}
}

// MatchModelPattern reports whether model matches pattern, where '*' matches
// zero or more characters:
//
//	"*-5" matches "gpt-5"
//	"gpt-*" matches "gpt-5" and "gpt-4"
//	"gemini-*-pro" matches "gemini-2.5-pro" and "gemini-3-pro".
func MatchModelPattern(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	model = strings.TrimSpace(model)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	// Iterative glob-style matcher supporting only '*' wildcard.
	pi, si := 0, 0
	starIdx := -1
	matchIdx := 0
	for si < len(model) {
		if pi < len(pattern) && (pattern[pi] == model[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = si
			pi++
			continue
		}
		if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
