package responses

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Gemini says why a turn ended in candidates[0].finishReason. Every reason used
// to end the Responses turn as completed, so a reply cut off by the token limit,
// withheld by a safety filter or broken off by a malformed function call reached
// the client as a finished answer, and an agent stopped there (#1144).

// geminiFinishMalformedFunctionCall is the finishReason Gemini gives when it set
// out to call a tool and could not write the call.
const geminiFinishMalformedFunctionCall = "MALFORMED_FUNCTION_CALL"

// geminiResponsesFinish is how a Gemini turn ends for a Responses client.
type geminiResponsesFinish struct {
	// status is "completed" or "incomplete", and names the terminal stream event.
	status string
	// incompleteReason is incomplete_details.reason; "" makes incomplete_details null.
	incompleteReason string
}

// geminiResponsesFinishOf reads the finishReason of a Gemini response. A token
// limit and the content filters map as they do for every other client
// (StopReasonFromGemini). Reasons with no settled meaning for a client, such as
// OTHER, LANGUAGE, UNEXPECTED_TOOL_CALL or TOO_MANY_TOOL_CALLS, still end the
// turn as completed.
func geminiResponsesFinishOf(modelName string, root gjson.Result) geminiResponsesFinish {
	candidate := root.Get("candidates.0")
	finishReason := candidate.Get("finishReason").String()
	if strings.EqualFold(finishReason, geminiFinishMalformedFunctionCall) {
		// What came before is not the turn the model meant to finish: often the
		// call written out as text, sometimes only thinking. The Antigravity
		// executor asks again when nothing visible was sent yet
		// (antigravity_malformed_call.go); this is what is left once it cannot.
		// Responses has no reason for it: incomplete_details.reason names only
		// max_output_tokens, max_messages, content_filter and steered, each of
		// which would send the client the wrong way, so there is none (see apply).
		// The request log shows a successful request, so the turn is logged; the
		// details go into the message, as the shared log formatter prints only a
		// fixed set of fields.
		log.Warnf("gemini responses: model %s ended the turn with %s, reported as incomplete: %.200q",
			modelName, geminiFinishMalformedFunctionCall, candidate.Get("finishMessage").String())
		return geminiResponsesFinish{status: "incomplete"}
	}
	status, reason := common.StopReasonFromGemini(finishReason).Responses()
	return geminiResponsesFinish{status: status, incompleteReason: reason}
}

// apply sets status and incomplete_details on a Responses object at prefix:
// "response." in a stream event, "" in a non-stream body.
//
// Without a reason incomplete_details is null, as the other converters write
// it, not an empty object: the Vercel AI SDK (and the clients built on it)
// validates the field as {reason: string} or null and fails the whole event on
// {}. Codex CLI and pi read a null and a missing reason alike.
func (f geminiResponsesFinish) apply(resp, prefix string) string {
	resp, _ = sjson.Set(resp, prefix+"status", f.status)
	if f.status != "incomplete" {
		return resp
	}
	details := "null"
	if f.incompleteReason != "" {
		details, _ = sjson.Set(`{}`, "reason", f.incompleteReason)
	}
	resp, _ = sjson.SetRaw(resp, prefix+"incomplete_details", details)
	return resp
}

// finishGeminiResponsesStream turns the final response, assembled as a
// response.completed event, into the event the finishReason calls for:
// response.completed, or response.incomplete for a reply that was cut short.
// Both end the stream normally for the completion tracker and the WebSocket
// handler, so neither counts against the account.
func finishGeminiResponsesStream(modelName string, root gjson.Result, completed string) string {
	finish := geminiResponsesFinishOf(modelName, root)
	eventType := "response." + finish.status
	completed, _ = sjson.Set(completed, "type", eventType)
	return emitEvent(eventType, finish.apply(completed, "response."))
}

// finishGeminiResponsesBody sets the status of a non-stream response from its
// finishReason.
func finishGeminiResponsesBody(modelName string, root gjson.Result, resp string) string {
	return geminiResponsesFinishOf(modelName, root).apply(resp, "")
}
