package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestPolicyAction string

const (
	requestPolicyActionSkipChannel requestPolicyAction = "skip-channel"
	requestPolicyActionReject      requestPolicyAction = "reject"
)

type requestPolicyLimitError struct {
	policy           string
	requestedModel   string
	upstreamProvider string
	upstreamModel    string
	requestBytes     int64
	maxRequestBytes  int64
	action           requestPolicyAction
}

func (e *requestPolicyLimitError) Error() string {
	if e == nil {
		return ""
	}
	message := fmt.Sprintf("request body is too large for upstream model %s via provider %s: %d bytes exceeds max-request-bytes %d",
		e.upstreamModel, e.upstreamProvider, e.requestBytes, e.maxRequestBytes)
	payload := map[string]any{
		"error": map[string]any{
			"message":           message,
			"type":              "invalid_request_error",
			"code":              "request_too_large",
			"policy":            e.policy,
			"requested_model":   e.requestedModel,
			"upstream_provider": e.upstreamProvider,
			"upstream_model":    e.upstreamModel,
			"request_bytes":     e.requestBytes,
			"max_request_bytes": e.maxRequestBytes,
			"over_limit_action": string(e.action),
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return message
	}
	return string(data)
}

func (e *requestPolicyLimitError) StatusCode() int {
	return http.StatusRequestEntityTooLarge
}

func (e *requestPolicyLimitError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return headers
}

func requestPolicyDecision(cfg *internalconfig.Config, auth *Auth, opts cliproxyexecutor.Options, requestedModel, upstreamProvider, upstreamModel string) (bool, *requestPolicyLimitError) {
	if cfg == nil || len(cfg.RequestPolicies) == 0 || auth == nil {
		return false, nil
	}
	requestBytes, ok := requestBytesFromMetadata(opts.Metadata)
	if !ok || requestBytes <= 0 {
		return false, nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	upstreamProvider = strings.ToLower(strings.TrimSpace(upstreamProvider))
	upstreamModel = strings.TrimSpace(upstreamModel)
	for i := range cfg.RequestPolicies {
		policy := cfg.RequestPolicies[i]
		maxBytes := policy.Limits.MaxRequestBytes
		if maxBytes <= 0 || requestBytes <= maxBytes {
			continue
		}
		if !requestPolicyMatches(policy, requestedModel, upstreamProvider, upstreamModel) {
			continue
		}
		action := requestPolicyAction(strings.ToLower(strings.TrimSpace(policy.OverLimit.Action)))
		if action == "" {
			action = requestPolicyActionSkipChannel
		}
		if action != requestPolicyActionReject {
			action = requestPolicyActionSkipChannel
		}
		return true, &requestPolicyLimitError{
			policy:           strings.TrimSpace(policy.Name),
			requestedModel:   requestedModel,
			upstreamProvider: upstreamProvider,
			upstreamModel:    upstreamModel,
			requestBytes:     requestBytes,
			maxRequestBytes:  maxBytes,
			action:           action,
		}
	}
	return false, nil
}

func requestPolicyMatches(policy internalconfig.RequestPolicy, requestedModel, upstreamProvider, upstreamModel string) bool {
	if !policyValuesMatchModel(policy.Match.RequestedModels, requestedModel) {
		return false
	}
	if !policyValuesMatchString(policy.Match.UpstreamProviders, upstreamProvider) {
		return false
	}
	if !policyValuesMatchModel(policy.Match.UpstreamModels, upstreamModel) {
		return false
	}
	return true
}

func policyValuesMatchString(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range values {
		if strings.EqualFold(strings.TrimSpace(candidate), value) {
			return true
		}
	}
	return false
}

func policyValuesMatchModel(values []string, model string) bool {
	if len(values) == 0 {
		return true
	}
	model = canonicalPolicyModel(model)
	for _, candidate := range values {
		if canonicalPolicyModel(candidate) == model {
			return true
		}
	}
	return false
}

func canonicalPolicyModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	if strings.TrimSpace(parsed.ModelName) != "" {
		model = parsed.ModelName
	}
	return strings.ToLower(strings.TrimSpace(model))
}

func requestBytesFromMetadata(meta map[string]any) (int64, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	raw, ok := meta[cliproxyexecutor.RequestBytesMetadataKey]
	if !ok || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case float64:
		return int64(v), true
	case json.Number:
		parsed, err := v.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
