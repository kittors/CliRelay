package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/payloadrules"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// payloadRulesDryRunRequest is the body of POST /payload-rules/dry-run.
type payloadRulesDryRunRequest struct {
	// Model is the upstream model the request resolves to.
	Model string `json:"model"`
	// RequestedModel is what the client asks for, alias and thinking suffix
	// included ("fast(high)").
	RequestedModel string `json:"requested-model"`
	// Protocol is the format of Body, as model entries of the rules name it.
	Protocol string `json:"protocol"`
	// Body is the sample request body: the JSON text in a string, or the JSON
	// object itself.
	Body json.RawMessage `json:"body"`
	// Rules replaces the saved rules section by section. A section left out,
	// or null, is the saved one, so the panel sends only the sections it edits.
	Rules *payloadRulesDraft `json:"rules"`
}

type payloadRulesDraft struct {
	Default     json.RawMessage `json:"default"`
	DefaultRaw  json.RawMessage `json:"default-raw"`
	Override    json.RawMessage `json:"override"`
	OverrideRaw json.RawMessage `json:"override-raw"`
	Filter      json.RawMessage `json:"filter"`
}

type payloadRulesDryRunResponse struct {
	// Body is the rewritten body, indented.
	Body string `json:"body"`
	// Models are the names the rules were matched against, in order.
	Models  []string                   `json:"models"`
	Rules   []payloadrules.RuleOutcome `json:"rules"`
	Changes []payloadrules.Change      `json:"changes"`
	// Dropped lists draft *-raw rules that saving would discard because a
	// value is not valid JSON. They did not run.
	Dropped []droppedPayloadRule `json:"dropped,omitempty"`
}

type droppedPayloadRule struct {
	Section payloadrules.Section `json:"section"`
	Index   int                  `json:"index"`
	Param   string               `json:"param"`
}

// PostPayloadRulesDryRun runs the payload rules on a sample body and returns
// the body a request would be sent with, what each rule did and what changed,
// without sending anything upstream. With a draft in "rules" a rule can be
// checked before it is saved.
//
// Rule paths are read against the body as sent. Gemini CLI and Antigravity wrap
// the request in a "request" field and apply the rules inside it, so for them
// the sample is the content of that field.
func (h *Handler) PostPayloadRulesDryRun(c *gin.Context) {
	raw, err := bodyutil.ReadRequestBody(c, bodyutil.ManagementBodyLimit)
	if err != nil {
		if bodyutil.IsTooLarge(err) {
			payloadDryRunError(c, http.StatusRequestEntityTooLarge, "payload_dry_run_too_large", "request body is too large")
			return
		}
		payloadDryRunError(c, http.StatusBadRequest, "payload_dry_run_invalid_request", "failed to read the request body")
		return
	}
	var req payloadRulesDryRunRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		payloadDryRunError(c, http.StatusBadRequest, "payload_dry_run_invalid_request", "the request body is not valid JSON")
		return
	}
	model := strings.TrimSpace(req.Model)
	requestedModel := strings.TrimSpace(req.RequestedModel)
	if model == "" && requestedModel == "" {
		payloadDryRunError(c, http.StatusBadRequest, "payload_dry_run_model_required", "model or requested-model is required")
		return
	}
	body, ok := payloadDryRunSampleBody(req.Body)
	if !ok {
		payloadDryRunError(c, http.StatusBadRequest, "payload_dry_run_invalid_body", "body must be a JSON object")
		return
	}

	rules := h.savedPayloadRules(c)
	var draftIndexes map[payloadrules.Section][]int
	var dropped []droppedPayloadRule
	if req.Rules != nil {
		rules, draftIndexes, dropped, err = mergePayloadRulesDraft(rules, *req.Rules)
		if err != nil {
			payloadDryRunError(c, http.StatusBadRequest, "payload_dry_run_invalid_rules", err.Error())
			return
		}
	}

	result := payloadrules.Explain(rules, payloadrules.Request{
		Model:          model,
		RequestedModel: requestedModel,
		Protocol:       strings.TrimSpace(req.Protocol),
		Body:           body,
	})
	// Indexes name the rules as the caller sent them, dropped ones included.
	for i := range result.Rules {
		if positions, ok := draftIndexes[result.Rules[i].Section]; ok {
			result.Rules[i].Index = positions[result.Rules[i].Index]
		}
	}
	response := payloadRulesDryRunResponse{
		Body:    indentedJSON(result.Body),
		Models:  result.Models,
		Rules:   result.Rules,
		Changes: result.Changes,
		Dropped: dropped,
	}
	if response.Models == nil {
		response.Models = []string{}
	}
	if response.Rules == nil {
		response.Rules = []payloadrules.RuleOutcome{}
	}
	if response.Changes == nil {
		response.Changes = []payloadrules.Change{}
	}
	c.JSON(http.StatusOK, response)
}

func payloadDryRunError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// savedPayloadRules are the rules the caller's tenant runs now.
func (h *Handler) savedPayloadRules(c *gin.Context) config.PayloadConfig {
	if h == nil || h.cfg == nil {
		return config.PayloadConfig{}
	}
	tenantID := effectiveTenantID(c)
	if tenantID == identity.SystemTenantID {
		return h.cfg.Payload
	}
	return usage.BuildTenantRuntimeConfig(h.cfg, tenantID).Payload
}

// payloadDryRunSampleBody accepts the sample as JSON text in a string or as the
// object itself, and requires an object: every upstream body is one.
func payloadDryRunSampleBody(raw json.RawMessage) ([]byte, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, false
		}
		raw = bytes.TrimSpace([]byte(text))
	}
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return nil, false
	}
	return raw, true
}

// mergePayloadRulesDraft replaces the saved sections the draft carries. Draft
// *-raw rules go through the same check saving applies, and the ones it would
// drop are left out and reported. draftIndexes maps each section's run
// position back to the draft's position; sections taken from the saved rules
// have no entry.
func mergePayloadRulesDraft(saved config.PayloadConfig, draft payloadRulesDraft) (rules config.PayloadConfig, draftIndexes map[payloadrules.Section][]int, dropped []droppedPayloadRule, err error) {
	rules = saved
	draftIndexes = make(map[payloadrules.Section][]int)
	valueSection := func(section payloadrules.Section, raw json.RawMessage, target *[]config.PayloadRule, checkRaw bool) error {
		if isAbsentJSON(raw) {
			return nil
		}
		var parsed []config.PayloadRule
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return fmt.Errorf("rules.%s: %v", section, err)
		}
		kept := make([]config.PayloadRule, 0, len(parsed))
		positions := make([]int, 0, len(parsed))
		for i, rule := range parsed {
			if checkRaw {
				if param, invalid := config.InvalidPayloadRawParam(rule); invalid {
					dropped = append(dropped, droppedPayloadRule{Section: section, Index: i, Param: param})
					continue
				}
			}
			kept = append(kept, rule)
			positions = append(positions, i)
		}
		*target = kept
		draftIndexes[section] = positions
		return nil
	}
	if err = valueSection(payloadrules.SectionDefault, draft.Default, &rules.Default, false); err != nil {
		return saved, nil, nil, err
	}
	if err = valueSection(payloadrules.SectionDefaultRaw, draft.DefaultRaw, &rules.DefaultRaw, true); err != nil {
		return saved, nil, nil, err
	}
	if err = valueSection(payloadrules.SectionOverride, draft.Override, &rules.Override, false); err != nil {
		return saved, nil, nil, err
	}
	if err = valueSection(payloadrules.SectionOverrideRaw, draft.OverrideRaw, &rules.OverrideRaw, true); err != nil {
		return saved, nil, nil, err
	}
	if !isAbsentJSON(draft.Filter) {
		var filters []config.PayloadFilterRule
		if err = json.Unmarshal(draft.Filter, &filters); err != nil {
			return saved, nil, nil, fmt.Errorf("rules.%s: %v", payloadrules.SectionFilter, err)
		}
		rules.Filter = filters
	}
	return rules, draftIndexes, dropped, nil
}

func isAbsentJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// indentedJSON indents a body for reading; numbers and strings keep their text.
func indentedJSON(body []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		return string(body)
	}
	return out.String()
}
