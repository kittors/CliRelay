package executor

import "net/http"

// newOpenAICompatStatusErr wraps a failed OpenAI-compatible response.
//
// It deliberately publishes no retry wait. The scheduler reads Retry-After, the
// rate-limit reset headers and the message text of every failed result itself,
// whichever executor produced it, and treats what it reads as advisory so that
// an account with disable-cooling still retries at once. A value published from
// here would be binding, which is only right for providers whose own format an
// executor parses (Codex, xAI, Gemini CLI).
func newOpenAICompatStatusErr(code int, body []byte, headers http.Header) statusErr {
	return statusErr{code: code, msg: string(body), headers: headers.Clone()}
}
