package loopdetect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ReplyLoopCode is the error code a client reads when its reply was cut.
const ReplyLoopCode = "reply_loop_detected"

// ReplyLoopStatus is the status a cut reply reports. It is a 4xx on purpose:
// Claude Code and the OpenAI SDKs retry a 5xx, 408 or 409 by themselves, and a
// reply that looped once is likely to loop again, which would pay for the same
// loop several times over before anyone saw the message.
const ReplyLoopStatus = http.StatusUnprocessableEntity

// replyLoopAdvice ends every message: what happened to the reply, and how to
// have it run to its end instead.
const replyLoopAdvice = `The reply was ended here instead of running on to its output limit. Ask again, or have an administrator turn off "Stop repeating replies" for this API key or tenant (Loop protection, in the management panel).`

// Message is what the client and the request log are told.
func (t ReplyTrip) Message() string {
	what := "answer"
	if t.Reasoning {
		what = "reasoning"
	}
	quoted := t.quoted()
	if t.RunOn {
		return fmt.Sprintf("Reply stopped by the gateway's loop protection: the model's %s was stuck in a loop, its last %d characters the same few words over and over with no break (%s). %s",
			what, t.Window, quoted, replyLoopAdvice)
	}
	return fmt.Sprintf("Reply stopped by the gateway's loop protection: the model's %s was stuck in a loop, its last %d lines and sentences all among the same %d (%s). %s",
		what, t.Window, t.Distinct, quoted, replyLoopAdvice)
}

// quoted lists the units that repeat, most frequent first.
func (t ReplyTrip) quoted() string {
	parts := make([]string, 0, maxQuoted+1)
	for i, unit := range t.Units {
		if i == maxQuoted {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%q", unit))
	}
	return strings.Join(parts, " ")
}

// ReplyLoopError is the error a cut reply ends with. Its text is an OpenAI-shaped
// error body, the form the handlers already take apart and rewrite into the
// client's own protocol for every upstream error (a Claude error event, a Gemini
// error status, a Responses error event), so the code survives into each of
// them.
type ReplyLoopError struct {
	Trip ReplyTrip
}

// Error implements error.
func (e *ReplyLoopError) Error() string {
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": e.Trip.Message(),
		"type":    "invalid_request_error",
		"code":    ReplyLoopCode,
	}})
	if err != nil {
		return e.Trip.Message()
	}
	return string(body)
}

// StatusCode implements the interface the handlers read a status from.
func (e *ReplyLoopError) StatusCode() int { return ReplyLoopStatus }
