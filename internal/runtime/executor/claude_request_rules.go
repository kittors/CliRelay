package executor

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Claude Opus 4.7 and later refuse parts of the request surface older models
// took: budget thinking, thinking.type "disabled" on some, sampling parameters,
// and on the newest a forced tool_choice. Clients written for older models and
// the Chat and Gemini translators still send them. The catalog records the rules
// of the models it knows (registry.ClaudeRequestRules, registry.AdaptiveThinking)
// and the request is shaped for them before it goes out.
//
// For models the catalog does not know (configured names, Haiku 5.5, Mythos,
// models released after this build) the upstream's 400 names the rule the
// request broke. The request is rewritten the way the error asks and sent again,
// and the rule is remembered for that upstream and model, so later requests are
// rewritten before they go out instead of paying for the 400 every time.

// claudeRejection is a rule of a Claude model's request surface that a request
// can break. Several are combined as a bit set.
type claudeRejection uint8

// The declaration order is the order prepare applies the fixes in: the thinking
// shape first, then the effort that shape allows.
const (
	// claudeRejectsBudgetThinking: thinking.type "enabled" with budget_tokens.
	claudeRejectsBudgetThinking claudeRejection = 1 << iota
	// claudeRejectsDisabledThinking: thinking.type "disabled", on a model that
	// cannot turn thinking off.
	claudeRejectsDisabledThinking
	// claudeRejectsDisabledUseBetweenTools: thinking.type "disabled", on a
	// model whose lowest setting is "between_tools" (Sonnet 5.5).
	claudeRejectsDisabledUseBetweenTools
	// claudeRejectsEffortWhenOff: an effort above "high" with thinking off.
	claudeRejectsEffortWhenOff
	// claudeRejectsSampling: temperature, top_p or top_k.
	claudeRejectsSampling
	// claudeRejectsForcedToolChoice: tool_choice "any" or "tool".
	claudeRejectsForcedToolChoice

	claudeLastRejection = claudeRejectsForcedToolChoice
)

// claudeThinkingRejections are the rules that only an adaptive-only model has.
const claudeThinkingRejections = claudeRejectsBudgetThinking | claudeRejectsDisabledThinking |
	claudeRejectsDisabledUseBetweenTools | claudeRejectsEffortWhenOff

func (r claudeRejection) String() string {
	names := []string{"budget thinking", "disabled thinking", "disabled thinking (use between_tools)", "effort above high with thinking off", "sampling parameters", "forced tool_choice"}
	var parts []string
	for i, name := range names {
		if r&(1<<i) != 0 {
			parts = append(parts, name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// Anthropic's wording, matched literally. The thinking and tool_choice messages
// are from the API documentation; the sampling one is what the API has returned
// for every parameter and model since Opus 4.7 (no documented text exists):
//
//	"thinking.type.enabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort" to control thinking behavior.
//	"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort" to control thinking behavior.
//	"thinking.type.disabled" is not supported for this model. Use "thinking.type.between_tools" for the lowest thinking setting, or ...
//	output_config.effort 'xhigh' is not supported when thinking is disabled on this model. Use effort 'high' or below, or enable thinking.
//	`temperature` is deprecated for this model.
//	tool_choice: type "tool" and "any" are not supported for this model.
const (
	claudeMsgEnabledThinking     = `"thinking.type.enabled" is not supported`
	claudeMsgDisabledThinking    = `"thinking.type.disabled" is not supported`
	claudeMsgBetweenTools        = `"thinking.type.between_tools"`
	claudeMsgEffortWhenOff       = `is not supported when thinking is disabled`
	claudeMsgParameterDeprecated = `is deprecated for this model`
	claudeMsgForcedToolChoice    = `"tool" and "any" are not supported`
)

// claudeSamplingFields are the request fields claudeRejectsSampling covers.
// The models that refuse one refuse all three, so all three are dropped.
var claudeSamplingFields = []string{"temperature", "top_p", "top_k"}

// classifyClaudeRejection reads which rule a 400 from a Claude upstream says
// the request broke, or 0 for any other error.
func classifyClaudeRejection(status int, errBody []byte) claudeRejection {
	if status != http.StatusBadRequest {
		return 0
	}
	// The message sits inside a JSON string, so its quotes arrive escaped.
	message := strings.ReplaceAll(string(errBody), `\"`, `"`)
	switch {
	case strings.Contains(message, claudeMsgEnabledThinking):
		return claudeRejectsBudgetThinking
	case strings.Contains(message, claudeMsgDisabledThinking):
		if strings.Contains(message, claudeMsgBetweenTools) {
			return claudeRejectsDisabledUseBetweenTools
		}
		return claudeRejectsDisabledThinking
	case strings.Contains(message, claudeMsgEffortWhenOff):
		return claudeRejectsEffortWhenOff
	case strings.Contains(message, claudeMsgForcedToolChoice):
		return claudeRejectsForcedToolChoice
	case strings.Contains(message, claudeMsgParameterDeprecated):
		for _, field := range claudeSamplingFields {
			if strings.Contains(message, "`"+field+"`") {
				return claudeRejectsSampling
			}
		}
	}
	return 0
}

// claudeFixContext is what the fixes need beyond the request body.
type claudeFixContext struct {
	// requested is the thinking level the client asked for
	// (ExecutionContext.RequestedThinkingLevel).
	requested string
	// instructTools adds an instruction to call the tool when a forced
	// tool_choice is relaxed; see relaxClaudeForcedToolChoice.
	instructTools bool
	// toUpstream is what the request body goes through on its way out (the
	// OAuth tool prefix), so an instruction names the tool the model sees.
	toUpstream func([]byte) []byte
}

// fixClaudeRejection rewrites body so it no longer breaks rule. ok is false
// when body does not have the shape the rule refuses, so there is nothing a
// rewrite could change.
func fixClaudeRejection(body []byte, rule claudeRejection, fc claudeFixContext) ([]byte, bool) {
	switch rule {
	case claudeRejectsBudgetThinking:
		return fixClaudeBudgetThinking(body, fc.requested)
	case claudeRejectsDisabledThinking:
		return fixClaudeDisabledThinking(body, false)
	case claudeRejectsDisabledUseBetweenTools:
		return fixClaudeDisabledThinking(body, true)
	case claudeRejectsEffortWhenOff:
		return fixClaudeEffortWhenOff(body)
	case claudeRejectsSampling:
		return stripClaudeSampling(body)
	case claudeRejectsForcedToolChoice:
		return relaxClaudeForcedToolChoice(body, fc)
	}
	return nil, false
}

// stripClaudeSampling removes temperature, top_p and top_k. Leaving them out
// is the default on every model, which is all the models that refuse them take.
func stripClaudeSampling(body []byte) ([]byte, bool) {
	changed := false
	for _, field := range claudeSamplingFields {
		if gjson.GetBytes(body, field).Exists() {
			body, _ = sjson.DeleteBytes(body, field)
			changed = true
		}
	}
	return body, changed
}

func claudeToolChoiceForced(body []byte) bool {
	switch gjson.GetBytes(body, "tool_choice.type").String() {
	case "any", "tool":
		return true
	}
	return false
}

// relaxClaudeForcedToolChoice turns a forced tool_choice into "auto" for a
// model that rejects forcing. disable_parallel_tool_use stays: with "auto" it
// still allows at most one call.
//
// "auto" lets the model answer without calling a tool, so the request also
// tells it to call one, which is what Anthropic recommends in place of forcing.
// The instruction is appended to the last user turn. That turn comes back
// without it on the next request, which changes the conversation prefix a
// thinking block records, and on these models the API then refuses (or drops)
// the block. Chat and Gemini clients never send Claude thinking blocks back, so
// they get the instruction; Claude and Responses clients do, so they get the
// plain "auto".
func relaxClaudeForcedToolChoice(body []byte, fc claudeFixContext) ([]byte, bool) {
	if !claudeToolChoiceForced(body) {
		return body, false
	}
	instruction := "Respond by calling one of the available tools."
	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		if name := claudeUpstreamToolChoiceName(body, fc.toUpstream); name != "" {
			instruction = fmt.Sprintf("Respond by calling the `%s` tool.", name)
		}
	}
	relaxed := `{"type":"auto"}`
	if parallel := gjson.GetBytes(body, "tool_choice.disable_parallel_tool_use"); parallel.Exists() {
		relaxed, _ = sjson.SetRaw(relaxed, "disable_parallel_tool_use", parallel.Raw)
	}
	out, _ := sjson.SetRawBytes(body, "tool_choice", []byte(relaxed))
	if fc.instructTools {
		out = appendClaudeUserInstruction(out, instruction)
	}
	return out, true
}

// claudeUpstreamToolChoiceName returns the tool_choice name as the upstream
// will see it. The OAuth tool prefix renames tools on the way out, except
// built-in ones and names that already carry it, so the same rename is run on
// just the tools and the tool_choice.
func claudeUpstreamToolChoiceName(body []byte, toUpstream func([]byte) []byte) string {
	name := gjson.GetBytes(body, "tool_choice.name").String()
	if name == "" || toUpstream == nil {
		return name
	}
	probe := []byte(`{}`)
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() {
		probe, _ = sjson.SetRawBytes(probe, "tools", []byte(tools.Raw))
	}
	probe, _ = sjson.SetRawBytes(probe, "tool_choice", []byte(gjson.GetBytes(body, "tool_choice").Raw))
	if upstream := gjson.GetBytes(toUpstream(probe), "tool_choice.name").String(); upstream != "" {
		return upstream
	}
	return name
}

// appendClaudeUserInstruction adds text as the last block of the last message
// when that message is the user's. Anything else (an assistant prefill, no
// messages) is left alone.
func appendClaudeUserInstruction(body []byte, text string) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	count := len(messages.Array())
	if count == 0 {
		return body
	}
	last := messages.Get(fmt.Sprintf("%d", count-1))
	if last.Get("role").String() != "user" {
		return body
	}
	block, _ := sjson.Set(`{"type":"text"}`, "text", text)
	path := fmt.Sprintf("messages.%d.content", count-1)
	content := last.Get("content")
	switch {
	case content.IsArray():
		out, _ := sjson.SetRawBytes(body, path+".-1", []byte(block))
		return out
	case content.Type == gjson.String:
		first, _ := sjson.Set(`{"type":"text"}`, "text", content.String())
		out, _ := sjson.SetRawBytes(body, path, []byte("["+first+","+block+"]"))
		return out
	}
	return body
}

// turnClaudeThinkingOffForForcedTool turns thinking off explicitly next to a
// forced tool_choice, for adaptive-only models that turn it off with
// "disabled" (Opus 4.7, 4.8 and 5, Sonnet 5). A forced call opens the reply,
// so the model does not think before it anyway. Removing thinking, as is done
// for older models, would turn it on for Opus 5 and Sonnet 5, and Bedrock
// rejects a forced tool_choice unless thinking is "disabled". "disabled" has an
// effort ceiling on some of these models (Opus 5: "high").
func turnClaudeThinkingOffForForcedTool(body []byte, adaptive *registry.AdaptiveThinking) []byte {
	if adaptive == nil || adaptive.Off != registry.ClaudeThinkingOffDisabled || !claudeToolChoiceForced(body) {
		return body
	}
	out, _ := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"disabled"}`))
	if adaptive.OffMaxLevel != "" {
		out = capClaudeEffortAt(out, thinking.ThinkingLevel(adaptive.OffMaxLevel))
	}
	return out
}

// claudeRequestPlan shapes one request for the request surface of the model it
// goes to: the catalog's rules, the rules earlier 400s from the same upstream
// named for the same model, and those the attempts of this request run into.
type claudeRequestPlan struct {
	// key identifies the upstream and model in claudeLearnedRules.
	key string
	// model is the upstream model, for logs.
	model string
	// adaptive is the catalog's thinking rule, nil for models it does not
	// list as adaptive-only.
	adaptive *registry.AdaptiveThinking
	// catalog holds the rules the catalog lists for the model.
	catalog claudeRejection
	// dropThinkingForForcedTool: see claudeRequestTarget.
	dropThinkingForForcedTool bool
	fix                       claudeFixContext

	// original is the request as it was before any rule was applied, known
	// the rules applied to it, and sent the body of the last attempt. Every
	// attempt is original shaped for known, so a retry sends what the next
	// request to the model will send.
	original []byte
	known    claudeRejection
	sent     []byte
}

// claudeRequestTarget is where a request goes, for newClaudeRequestPlan.
type claudeRequestTarget struct {
	provider string
	// endpoint is the URL the request is posted to.
	endpoint string
	// model is the upstream model.
	model string
	// catalogModel is the catalog id to read rules from: the upstream model
	// itself for the Claude API, the Anthropic id behind a Bedrock id for
	// Bedrock.
	catalogModel string
	// toUpstream is what the body goes through on its way out (the OAuth
	// tool prefix), or nil.
	toUpstream func([]byte) []byte
	// dropThinkingForForcedTool removes thinking next to a forced tool_choice
	// for models the catalog does not describe, which older Claude models
	// require. Off for upstreams with skip_anthropic_processing, which have
	// always received thinking as the client sent it.
	dropThinkingForForcedTool bool
}

func newClaudeRequestPlan(execCtx *ExecutionContext, target claudeRequestTarget) *claudeRequestPlan {
	plan := &claudeRequestPlan{
		key:                       claudeLearnedRuleKey(target.provider, target.endpoint, target.model),
		model:                     target.model,
		dropThinkingForForcedTool: target.dropThinkingForForcedTool,
		fix: claudeFixContext{
			requested:     execCtx.RequestedThinkingLevel(),
			instructTools: !claudeThinkingRoundTrips(execCtx.SourceFormat.String()),
			toUpstream:    target.toUpstream,
		},
	}
	if info := registry.LookupClaudeModelInfo(target.catalogModel); info != nil {
		plan.adaptive = thinking.AdaptiveThinkingOf(info)
		if rules := info.ClaudeRequest; rules != nil {
			if rules.NoSampling {
				plan.catalog |= claudeRejectsSampling
			}
			if rules.NoForcedToolChoice {
				plan.catalog |= claudeRejectsForcedToolChoice
			}
		}
	}
	return plan
}

// claudeThinkingRoundTrips reports whether clients of sourceFormat send Claude
// thinking blocks back on the next request. The Claude protocol does, and so
// does Responses, which carries their signatures in encrypted_content (see
// the claude/openai/responses translator). Chat and Gemini have no place for
// them.
func claudeThinkingRoundTrips(sourceFormat string) bool {
	switch sourceFormat {
	case "claude", "openai-response":
		return true
	}
	return false
}

// begin returns body shaped for every rule known for the model, for the first
// attempt.
func (p *claudeRequestPlan) begin(execCtx *ExecutionContext, body []byte) []byte {
	p.original = body
	p.known = p.catalog | claudeLearnedRules.get(p.key)
	p.sent = p.shape(body, p.known)
	if p.known != 0 && !bytes.Equal(p.sent, body) {
		logWithRequestID(execCtx.Context).Debugf("claude request rules: model %s refuses %s; shaped the request before sending", p.model, p.known)
	}
	return p.sent
}

// retry reads the error of a failed attempt. When it is a 400 naming a rule
// the request broke (see classifyClaudeRejection) and shaping the request for
// that rule changes it, retry returns the request shaped for it as well and
// remembers the rule for the model. A rule already applied is not retried,
// which bounds the attempts.
func (p *claudeRequestPlan) retry(execCtx *ExecutionContext, status int, errBody []byte) ([]byte, bool) {
	rule := classifyClaudeRejection(status, errBody)
	if rule == 0 || p.known&rule != 0 {
		return nil, false
	}
	next := p.shape(p.original, p.known|rule)
	if bytes.Equal(next, p.sent) {
		return nil, false
	}
	p.known |= rule
	p.sent = next
	logger := logWithRequestID(execCtx.Context)
	if claudeLearnedRules.add(p.key, rule) {
		logger.Infof("claude request rules: upstream refused %s for model %s; retrying, and shaping later requests to it before sending", rule, p.model)
	} else {
		logger.Debugf("claude request rules: upstream refused %s for model %s; retrying with the request shaped for it", rule, p.model)
	}
	return next, true
}

// knownAdaptive reports whether the model only takes adaptive thinking, from
// the catalog or from a 400 it returned. It is meant for after begin.
func (p *claudeRequestPlan) knownAdaptive() bool {
	return p.adaptive != nil || p.known&claudeThinkingRejections != 0
}

// shape applies rules to body in declaration order. A forced tool_choice that
// is left then gets the thinking it needs: "disabled" on adaptive-only models
// that turn thinking off that way (turnClaudeThinkingOffForForcedTool), and
// for models the catalog does not describe, no thinking, which older Claude
// models require next to forcing. Removing it is never worse than keeping it
// there: models that think by default still do.
func (p *claudeRequestPlan) shape(body []byte, rules claudeRejection) []byte {
	for rule := claudeRejection(1); rule != 0 && rule <= claudeLastRejection; rule <<= 1 {
		if rules&rule == 0 {
			continue
		}
		if fixed, ok := fixClaudeRejection(body, rule, p.fix); ok {
			body = fixed
		}
	}
	if !claudeToolChoiceForced(body) {
		return body
	}
	if p.adaptive != nil {
		return turnClaudeThinkingOffForForcedTool(body, p.adaptive)
	}
	if p.dropThinkingForForcedTool {
		return disableThinkingIfToolChoiceForced(body)
	}
	return body
}

// claudeLearnedRuleKey keys learned rules by provider, upstream host and
// model: two upstreams may serve different models under one name.
func claudeLearnedRuleKey(provider, endpoint, model string) string {
	host := endpoint
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return strings.ToLower(provider + "|" + host + "|" + strings.TrimSpace(model))
}

const (
	// claudeLearnedRuleTTL lets a learned rule lapse, so a model that
	// changes is asked again.
	claudeLearnedRuleTTL = 12 * time.Hour
	// claudeLearnedRuleLimit bounds the cache. Keys come from configured
	// upstreams and model names, which are few.
	claudeLearnedRuleLimit = 1024
)

// claudeLearnedRuleCache holds what 400s taught about models the catalog
// does not know. It is per process and starts empty.
type claudeLearnedRuleCache struct {
	mu      sync.Mutex
	entries map[string]claudeLearnedRule
	now     func() time.Time
}

type claudeLearnedRule struct {
	rules   claudeRejection
	expires time.Time
}

var claudeLearnedRules = newClaudeLearnedRuleCache()

func newClaudeLearnedRuleCache() *claudeLearnedRuleCache {
	return &claudeLearnedRuleCache{entries: make(map[string]claudeLearnedRule), now: time.Now}
}

func (c *claudeLearnedRuleCache) get(key string) claudeRejection {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return 0
	}
	if !c.now().Before(entry.expires) {
		delete(c.entries, key)
		return 0
	}
	return entry.rules
}

// add records rule for key and reports whether it was new.
func (c *claudeLearnedRuleCache) add(key string, rule claudeRejection) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	entry, ok := c.entries[key]
	if ok && !now.Before(entry.expires) {
		entry, ok = claudeLearnedRule{}, false
	}
	if !ok && len(c.entries) >= claudeLearnedRuleLimit {
		c.evictLocked(now)
	}
	isNew := entry.rules&rule == 0
	c.entries[key] = claudeLearnedRule{rules: entry.rules | rule, expires: now.Add(claudeLearnedRuleTTL)}
	return isNew
}

// evictLocked drops expired entries, and the one closest to expiring if that
// frees nothing.
func (c *claudeLearnedRuleCache) evictLocked(now time.Time) {
	oldestKey, oldest := "", time.Time{}
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
			continue
		}
		if oldestKey == "" || entry.expires.Before(oldest) {
			oldestKey, oldest = key, entry.expires
		}
	}
	if len(c.entries) >= claudeLearnedRuleLimit && oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
