package loopdetect

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// Thresholds of the tool-loop check.
const (
	// DefaultToolRepeats is how many turns in a row of one call answered with
	// one result make a loop: the request that would be the next is answered by
	// the gateway instead of the model.
	DefaultToolRepeats = 8
	MinToolRepeats     = 3
	MaxToolRepeats     = 32

	// tailPerRepeat is how many of a conversation's newest items are read per
	// repeat: a turn is two messages, or seven Responses items with a reasoning
	// item and three parallel calls, and the rest is slack.
	tailPerRepeat = 8
	// maxNoticeCalls is how many of a turn's calls the notice names.
	maxNoticeCalls = 3
)

// ToolLimits are the thresholds of the tool-loop check.
type ToolLimits struct {
	// Repeats: this many turns in a row, each the same calls answered with the
	// same results, are a loop.
	Repeats int `json:"repeats"`
}

// Normalized returns l with an unset Repeats defaulted and held to its bounds.
func (l ToolLimits) Normalized() ToolLimits {
	if l.Repeats <= 0 {
		l.Repeats = DefaultToolRepeats
	}
	l.Repeats = min(max(l.Repeats, MinToolRepeats), MaxToolRepeats)
	return l
}

// ToolCall is one call of the repeated turn.
type ToolCall struct {
	Name string
	// Arguments are the call's arguments cut short.
	Arguments string
}

// ToolLoop is what the check found: the newest turns of a conversation are one
// exchange, repeated.
type ToolLoop struct {
	// Turns is how many turns in a row ended the conversation with the same
	// calls and results.
	Turns int
	// Repeats is the threshold that was applied.
	Repeats int
	// Calls are the calls of the repeated turn.
	Calls []ToolCall
	// Items is how many items the conversation holds and Read how many of its
	// newest were looked at.
	Items int
	Read  int
}

// DetectToolLoop says whether the conversation in body, a request of protocol
// p, ends in a tool-call loop: its last Repeats turns are all one exchange, the
// same calls with the same arguments answered with the same results. A turn
// that got a different result is progress and ends the count, and so does
// anything the client side says beside the results.
//
// Only the newest items of the conversation are read, found without decoding
// the rest: an agent's request grows with every turn and the check runs on all
// of them.
func DetectToolLoop(p Protocol, body []byte, limits ToolLimits) (ToolLoop, bool) {
	limits = limits.Normalized()
	path := p.conversationPath()
	if path == "" || !mayLoop(p, body, limits.Repeats) {
		return ToolLoop{}, false
	}
	list := gjson.GetBytes(body, path)
	if !list.IsArray() {
		return ToolLoop{}, false
	}
	items, total := tailOf(list, tailPerRepeat*limits.Repeats)
	turns := exchanges(readMessages(p, items))
	if len(turns) < limits.Repeats {
		return ToolLoop{}, false
	}
	last := turns[len(turns)-1]
	n := 0
	for i := len(turns) - 1; i >= 0 && turns[i].sum == last.sum; i-- {
		n++
	}
	if n < limits.Repeats {
		return ToolLoop{}, false
	}
	return ToolLoop{Turns: n, Repeats: limits.Repeats, Calls: last.calls, Items: total, Read: len(items)}, true
}

// mayLoop says whether body holds results enough for a loop, counting them
// without parsing: most requests are no agent's, and the rest are mostly early
// in their conversation.
func mayLoop(p Protocol, body []byte, repeats int) bool {
	n := 0
	for _, mark := range p.resultMarks() {
		n += bytes.Count(body, []byte(mark))
		if n >= repeats {
			return true
		}
	}
	return false
}

// tailOf returns the last n items of list, oldest first, and the number of items
// the list holds.
func tailOf(list gjson.Result, n int) ([]gjson.Result, int) {
	ring := make([]gjson.Result, n)
	total := 0
	list.ForEach(func(_, item gjson.Result) bool {
		ring[total%n] = item
		total++
		return true
	})
	if total <= n {
		return ring[:total], total
	}
	start := total % n
	return append(ring[start:], ring[:start]...), total
}

// exchange is one turn of tool use: the calls the model made and the results
// the client sent back, summed without their ids, which differ every turn.
type exchange struct {
	sum   uint64
	calls []ToolCall
}

// exchanges are the turns of tool use msgs end in, oldest first, back to the
// newest turn that was not one: a model turn with no call, or a client turn with
// no result. Consecutive messages of one side are one turn (a Responses
// reasoning item and its call, a call's several outputs, a Chat call's tool
// messages).
func exchanges(msgs []message) []exchange {
	type run struct {
		side  side
		parts []part
	}
	var runs []run
	for _, m := range msgs {
		if n := len(runs); n > 0 && runs[n-1].side == m.side {
			runs[n-1].parts = append(runs[n-1].parts, m.parts...)
			continue
		}
		runs = append(runs, run{m.side, slices.Clone(m.parts)})
	}
	var out []exchange
	for i := len(runs) - 1; i >= 1; i -= 2 {
		said, asked := runs[i], runs[i-1]
		if said.side != sideUser || asked.side != sideModel {
			break
		}
		e, ok := exchangeOf(asked.parts, said.parts)
		if !ok {
			break
		}
		out = append(out, e)
	}
	slices.Reverse(out)
	return out
}

// exchangeOf sums one turn: each call by its name and arguments with the results
// the client gave it, and anything else the client side said beside the results.
// It is false for a turn that was not one.
func exchangeOf(asked, said []part) (exchange, bool) {
	var calls, results, rest []part
	for _, p := range asked {
		if p.kind == partCall {
			calls = append(calls, p)
		}
	}
	for _, p := range said {
		if p.kind == partResult {
			results = append(results, p)
		} else {
			rest = append(rest, p)
		}
	}
	if len(calls) == 0 || len(results) == 0 {
		return exchange{}, false
	}
	used := make([]bool, len(results))
	keys := make([]uint64, 0, len(calls)+1)
	shown := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		var h hasher
		h.add(sumOf(c.name))
		h.add(c.sum)
		for i, r := range results {
			if used[i] || !answers(c, r) {
				continue
			}
			used[i] = true
			h.add(r.sum)
			if c.id == "" || r.id == "" {
				break // matched by place: one result to a call
			}
		}
		keys = append(keys, h.sum)
		shown = append(shown, ToolCall{Name: c.name, Arguments: c.shown})
	}
	for i, r := range results { // results of no call here
		if !used[i] {
			keys = append(keys, sumOf("orphan")^r.sum)
		}
	}
	// the order calls were made in, or answered in, is no progress
	slices.Sort(keys)
	var all hasher
	for _, k := range keys {
		all.add(k)
	}
	for _, p := range rest {
		all.add(p.sum ^ 0xa5a5a5a5a5a5a5a5)
	}
	return exchange{sum: all.sum, calls: shown}, true
}

// answers says whether result r is the client's answer to call c: by id where
// both have one, else by the tool's name where both have one, else by place.
func answers(c, r part) bool {
	switch {
	case c.id != "" && r.id != "":
		return c.id == r.id
	case c.name != "" && r.name != "":
		return c.name == r.name
	}
	return true
}

// hasher folds sums into one, in order.
type hasher struct{ sum uint64 }

func (h *hasher) add(v uint64) {
	h.sum = (h.sum^v)*1099511628211 + 14695981039346656037
}

// called is the repeated turn's calls as a notice names them.
func (l ToolLoop) called() string {
	names := make([]string, 0, maxNoticeCalls+1)
	for i, c := range l.Calls {
		if i == maxNoticeCalls {
			names = append(names, "…")
			break
		}
		if c.Arguments == "" {
			names = append(names, c.Name)
		} else {
			names = append(names, c.Name+" "+c.Arguments)
		}
	}
	return strings.Join(names, "; ")
}

// Detail is what the request log says of the turn the gateway answered.
func (l ToolLoop) Detail() string {
	return fmt.Sprintf("tool loop: the last %d turns in a row were the same tool call with the same result (%s); threshold %d; answered by the gateway, nothing was sent upstream",
		l.Turns, l.called(), l.Repeats)
}

// toolLoopAdvice ends the notice: how to go on, and how to turn the check off.
const toolLoopAdvice = `Tell the agent how to go on and the conversation continues, or have an administrator turn off "Stop tool-call loops" for this API key or tenant (Loop protection, in the management panel).`

// Notice is what the client is told, as the turn's answer: a finished assistant
// message with no tool call in it, which ends the agent's turn.
func (l ToolLoop) Notice() string {
	return fmt.Sprintf("Stopped by the gateway's loop protection before this turn reached the model: the agent has run the same tool call %d turns in a row and got the same result every time (%s). The model is stuck repeating it, and another turn would spend quota with no progress, so this request was sent nowhere. %s",
		l.Turns, l.called(), toolLoopAdvice)
}
