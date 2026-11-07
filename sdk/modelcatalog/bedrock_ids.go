package modelcatalog

import (
	"strings"
	"unicode"
)

// ClaudeModelCandidatesForBedrockID returns the Anthropic model ids a Bedrock
// model id may stand for, most specific first, or nil when modelID is not an
// Anthropic model on Bedrock.
//
// Requests and configuration may name a Claude model by its Bedrock id, such as
// "us.anthropic.claude-opus-4-7-v1" or
// "global.anthropic.claude-sonnet-4-5-20250929-v1:0". The catalog lists the
// Anthropic id, and what the catalog says about the model (adaptive-only
// thinking, rejected request fields) decides whether Bedrock accepts the
// request, so the id is reduced to its Anthropic form: the ARN path, the
// cross-region prefix and "anthropic." are dropped, then the "-v1" / ":0"
// version. A dated id also yields its undated form, for catalogs that list
// only that.
//
// Application inference profile ARNs name no model and give nil.
func ClaudeModelCandidatesForBedrockID(modelID string) []string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if strings.HasPrefix(id, "arn:") {
		// arn:aws:bedrock:<region>:<account>:inference-profile/us.anthropic...
		// arn:aws:bedrock:<region>::foundation-model/anthropic...
		slash := strings.LastIndex(id, "/")
		if slash < 0 {
			return nil
		}
		id = id[slash+1:]
	}
	const vendor = "anthropic."
	at := strings.Index(id, vendor)
	if at < 0 || !isBedrockRegionPrefix(id[:at]) {
		return nil
	}
	id = id[at+len(vendor):]
	// Version: "-v1", "-v1:0", "-v2:0", and old ids with a context suffix
	// such as ":0:200k".
	for {
		colon := strings.LastIndex(id, ":")
		if colon < 0 {
			break
		}
		id = id[:colon]
	}
	if dash := strings.LastIndex(id, "-v"); dash > 0 && isDigits(id[dash+2:]) {
		id = id[:dash]
	}
	if !strings.HasPrefix(id, "claude") {
		return nil
	}
	candidates := []string{id}
	if dash := strings.LastIndex(id, "-"); dash > 0 && len(id)-dash-1 == 8 && isDigits(id[dash+1:]) {
		candidates = append(candidates, id[:dash])
	}
	return candidates
}

// isBedrockRegionPrefix accepts what may precede "anthropic." in a Bedrock id:
// nothing, or one cross-region inference profile prefix such as "us.",
// "apac." or "us-gov.".
func isBedrockRegionPrefix(prefix string) bool {
	if prefix == "" {
		return true
	}
	if !strings.HasSuffix(prefix, ".") {
		return false
	}
	region := strings.TrimSuffix(prefix, ".")
	if region == "" {
		return false
	}
	for _, r := range region {
		if r != '-' && !unicode.IsLower(r) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
