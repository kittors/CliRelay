// Package loopdetect finds a model that is stuck in a loop, in two places:
//
//   - ReplyGuard reads a streamed reply as it goes out to the client and says
//     when its reasoning or its text has become the same few lines over and
//     over, so the gateway can end the reply instead of paying for it to run to
//     the output limit.
//   - DetectToolLoop reads the conversation of a request and says when its last
//     turns are one tool call answered with one result again and again, so the
//     gateway can answer that turn itself and send nothing upstream.
//
// Both work on the protocol the client speaks (Chat Completions, Responses,
// Claude Messages, Gemini), never on the upstream's, because the gateway
// translates before it forwards. The package depends on nothing of the gateway:
// the settings behind it live in internal/loopguard, and the handlers apply
// the verdicts.
//
// Both checks are off unless a Policy turns them on, and a tripped check is a
// verdict on the model's output, not on the account that served it: it never
// feeds the scheduler's cooldowns.
//
// The ideas (units of lines and sentences counted in a window, a window of
// turns hashed without their ids) follow magpie's gateway loop guard
// (github.com/yetone/magpie, MIT, internal/gateway/loop_guard.go and
// tool_loop.go); the code, the protocol readers and the thresholds' bounds are
// this repository's own.
package loopdetect
