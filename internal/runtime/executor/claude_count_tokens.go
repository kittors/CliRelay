package executor

import "github.com/tidwall/sjson"

// stripClaudeCountTokensMetadata removes the top-level "metadata" object from a
// count_tokens request body.
//
// The Messages endpoint accepts metadata (the identity fingerprint writes
// metadata.user_id there), but /v1/messages/count_tokens does not: Anthropic
// rejects the request with 400 "metadata: Extra inputs are not permitted".
// Claude Code itself never sends metadata on count_tokens, so the field is
// dropped here whether the fingerprint injected it or the client forwarded it.
// The identity still travels in the headers. The Messages path is untouched.
func stripClaudeCountTokensMetadata(body []byte) []byte {
	out, err := sjson.DeleteBytes(body, "metadata")
	if err != nil {
		return body
	}
	return out
}
