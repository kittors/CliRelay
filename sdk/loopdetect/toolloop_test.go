package loopdetect

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// toolCall is one call of a round of tool use as a test writes it.
type toolCall struct {
	name  string
	args  string // the arguments' JSON as the client wrote them
	out   string // what the tool gave back
	isErr bool
	// outs are further results to the same call (a Responses call may have
	// several outputs).
	outs []string
}

// round is one turn of an agent: the model's calls and what came back.
type round struct {
	calls   []toolCall
	text    string // the model's words around its calls, worded differently by every round of a loop
	thought string // its reasoning
	user    string // words the client side adds beside the results
}

func readFile(path string) toolCall {
	return toolCall{name: "Read", args: fmt.Sprintf(`{"file_path":%q}`, path), out: "package main\n\nfunc main() {}\n"}
}

// same is n rounds of r.
func same(n int, r round) []round {
	out := make([]round, n)
	for i := range out {
		out[i] = r
	}
	return out
}

// writer writes the request a client sends after rounds, in one API.
type writer struct {
	name string
	p    Protocol
	// write: open leaves the last round's calls unanswered, as a request to
	// continue an agent's turn never is.
	write func(prompt string, rounds []round, open bool) []byte
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func objectOf(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	return v
}

var writers = []writer{
	{"claude", ProtocolClaude, func(prompt string, rounds []round, open bool) []byte {
		msgs := []any{map[string]any{"role": "user", "content": prompt}}
		for i, r := range rounds {
			var asked, said []any
			if r.thought != "" {
				asked = append(asked, map[string]any{"type": "thinking", "thinking": r.thought, "signature": fmt.Sprintf("sig%d", i)})
			}
			if r.text != "" {
				asked = append(asked, map[string]any{"type": "text", "text": r.text})
			}
			for j, c := range r.calls {
				id := fmt.Sprintf("toolu_%d_%d", i, j)
				asked = append(asked, map[string]any{"type": "tool_use", "id": id, "name": c.name, "input": objectOf(c.args)})
				block := map[string]any{"type": "tool_result", "tool_use_id": id, "content": []any{map[string]any{"type": "text", "text": c.out}}}
				if j%2 == 1 {
					block["content"] = c.out // both forms are written
				}
				if c.isErr {
					block["is_error"] = true
				}
				said = append(said, block)
			}
			if r.user != "" {
				said = append(said, map[string]any{"type": "text", "text": r.user})
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": asked})
			if !(open && i == len(rounds)-1) {
				msgs = append(msgs, map[string]any{"role": "user", "content": said})
			}
		}
		return []byte(jsonOf(map[string]any{"model": "claude-sonnet", "max_tokens": 1024, "stream": true, "messages": msgs}))
	}},
	{"chat", ProtocolChat, func(prompt string, rounds []round, open bool) []byte {
		msgs := []any{
			map[string]any{"role": "system", "content": "You are an agent."},
			map[string]any{"role": "user", "content": prompt},
		}
		for i, r := range rounds {
			var calls []any
			for j, c := range r.calls {
				calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d_%d", i, j), "type": "function",
					"function": map[string]any{"name": c.name, "arguments": c.args}})
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": nilIfEmpty(r.text), "tool_calls": calls})
			if open && i == len(rounds)-1 {
				continue
			}
			for j, c := range r.calls {
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d_%d", i, j), "content": c.out})
			}
			if r.user != "" {
				msgs = append(msgs, map[string]any{"role": "user", "content": r.user})
			}
		}
		return []byte(jsonOf(map[string]any{"model": "gpt", "stream": true, "messages": msgs}))
	}},
	{"responses", ProtocolResponses, func(prompt string, rounds []round, open bool) []byte {
		input := []any{map[string]any{"role": "user", "content": prompt}}
		for i, r := range rounds {
			if r.thought != "" {
				input = append(input, map[string]any{"type": "reasoning", "id": fmt.Sprintf("rs_%d", i), "summary": []any{}, "encrypted_content": r.thought})
			}
			if r.text != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": r.text}}})
			}
			for j, c := range r.calls {
				input = append(input, map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call_%d_%d", i, j), "name": c.name, "arguments": c.args})
			}
			if open && i == len(rounds)-1 {
				continue
			}
			for j, c := range r.calls {
				id := fmt.Sprintf("call_%d_%d", i, j)
				input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": c.out})
				for _, more := range c.outs {
					input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": more})
				}
			}
			if r.user != "" {
				input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": r.user}}})
			}
		}
		return []byte(jsonOf(map[string]any{"model": "gpt", "stream": true, "input": input}))
	}},
	{"gemini", ProtocolGemini, func(prompt string, rounds []round, open bool) []byte {
		contents := []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": prompt}}}}
		for _, r := range rounds {
			var asked, said []any
			if r.thought != "" {
				asked = append(asked, map[string]any{"text": r.thought, "thought": true})
			}
			if r.text != "" {
				asked = append(asked, map[string]any{"text": r.text})
			}
			for _, c := range r.calls {
				asked = append(asked, map[string]any{"functionCall": map[string]any{"name": c.name, "args": objectOf(c.args)}})
				said = append(said, map[string]any{"functionResponse": map[string]any{"name": c.name, "response": map[string]any{"output": c.out}}})
			}
			if r.user != "" {
				said = append(said, map[string]any{"text": r.user})
			}
			contents = append(contents, map[string]any{"role": "model", "parts": asked},
				map[string]any{"role": "user", "parts": said})
		}
		if open {
			contents = contents[:len(contents)-1] // the last round's calls have no answer yet
		}
		return []byte(jsonOf(map[string]any{"contents": contents}))
	}},
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// eachProtocol runs f against every API.
func eachProtocol(t *testing.T, f func(t *testing.T, w writer)) {
	t.Helper()
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) { f(t, w) })
	}
}

func detect(w writer, rounds []round, repeats int) (ToolLoop, bool) {
	return DetectToolLoop(w.p, w.write("fix the build", rounds, false), ToolLimits{Repeats: repeats})
}

func TestToolLoopIsFoundAfterTheLimitInEveryAPI(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		read := round{calls: []toolCall{readFile("/repo/main.go")}}
		for _, n := range []int{8, 9, 20} {
			loop, ok := detect(w, same(n, read), 8)
			if !ok || loop.Turns != n || loop.Repeats != 8 {
				t.Fatalf("%d identical rounds: %+v %v", n, loop, ok)
			}
			if len(loop.Calls) != 1 || loop.Calls[0].Name != "Read" || !strings.Contains(loop.Calls[0].Arguments, "/repo/main.go") {
				t.Errorf("calls %+v", loop.Calls)
			}
			if loop.Read == 0 || loop.Items < loop.Read {
				t.Errorf("read %d of %d items", loop.Read, loop.Items)
			}
		}
		if loop, ok := detect(w, same(7, read), 8); ok {
			t.Fatalf("7 identical rounds are under the limit: %+v", loop)
		}
		// a limit of the default when none is set
		if _, ok := DetectToolLoop(w.p, w.write("p", same(8, read), false), ToolLimits{}); !ok {
			t.Error("the default limit is 8")
		}
		if _, ok := DetectToolLoop(w.p, w.write("p", same(7, read), false), ToolLimits{}); ok {
			t.Error("7 rounds tripped the default limit")
		}
	})
}

func TestToolLoopIgnoresWhatDiffersEveryRound(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		// ids differ in every round (the writers number them); so do the model's
		// words and reasoning
		rounds := make([]round, 10)
		for i := range rounds {
			rounds[i] = round{
				calls:   []toolCall{readFile("/repo/main.go")},
				text:    fmt.Sprintf("Let me look at that file once more (attempt %d).", i),
				thought: fmt.Sprintf("thinking %d", i),
			}
		}
		if loop, ok := detect(w, rounds, 8); !ok || loop.Turns != 10 {
			t.Fatalf("%+v %v", loop, ok)
		}
	})
}

func TestToolLoopReadsArgumentsAsValues(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		// the same arguments written with their keys in another order, and with
		// other spacing, are the same
		rounds := make([]round, 9)
		for i := range rounds {
			args := `{"command":"go test ./...","timeout":120,"cwd":"/repo"}`
			if i%2 == 1 {
				args = `{ "cwd": "/repo", "timeout": 120, "command": "go test ./..." }`
			}
			rounds[i] = round{calls: []toolCall{{name: "Bash", args: args, out: "FAIL"}}}
		}
		if loop, ok := detect(w, rounds, 8); !ok || loop.Turns != 9 {
			t.Fatalf("%+v %v", loop, ok)
		}
		// other values are another call
		rounds[8].calls[0].args = `{"command":"go test ./...","timeout":121,"cwd":"/repo"}`
		if loop, ok := detect(w, rounds, 8); ok {
			t.Fatalf("a changed argument is progress: %+v", loop)
		}
	})
}

func TestToolLoopEndsWithAnyProgress(t *testing.T) {
	read := round{calls: []toolCall{readFile("/repo/main.go")}}
	for name, tc := range map[string]struct {
		change func(rounds []round) []round
		turns  int // 0: not a loop
	}{
		"the last result differs": {func(r []round) []round {
			r[len(r)-1].calls = []toolCall{{name: "Read", args: readFile("/repo/main.go").args, out: "package main\n\nfunc main() { run() }\n"}}
			return r
		}, 0},
		"a result in the middle differs, the last 10 are alike": {func(r []round) []round {
			r[3].calls = []toolCall{{name: "Read", args: readFile("/repo/main.go").args, out: "changed"}}
			return r
		}, 10},
		"a result inside the last 8 differs": {func(r []round) []round {
			r[len(r)-4].calls = []toolCall{{name: "Read", args: readFile("/repo/main.go").args, out: "changed"}}
			return r
		}, 0},
		// the run the newest turns make is what counts: 7 are one short of a limit
		// of 8, and 8 are the limit
		"the run is one turn short of the limit": {func(r []round) []round {
			r[len(r)-8].calls = []toolCall{{name: "Read", args: readFile("/repo/main.go").args, out: "changed"}}
			return r
		}, 0},
		"the run is exactly the limit": {func(r []round) []round {
			r[len(r)-9].calls = []toolCall{{name: "Read", args: readFile("/repo/main.go").args, out: "changed"}}
			return r
		}, 8},
		"the call differs": {func(r []round) []round {
			r[len(r)-2].calls = []toolCall{readFile("/repo/other.go")}
			return r
		}, 0},
		"the tool differs": {func(r []round) []round {
			r[len(r)-2].calls = []toolCall{{name: "Cat", args: readFile("/repo/main.go").args, out: readFile("x").out}}
			return r
		}, 0},
		"the user adds words to the last turn": {func(r []round) []round {
			r[len(r)-1].user = "no, stop reading that file"
			return r
		}, 0},
		"the user added words long ago": {func(r []round) []round {
			r[1].user = "keep going"
			return r
		}, 12},
		"a result is an error once": {func(r []round) []round {
			r[len(r)-1].calls[0].isErr = true
			return r
		}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			eachProtocol(t, func(t *testing.T, w writer) {
				rounds := same(14, read)
				for i := range rounds {
					rounds[i].calls = append([]toolCall(nil), rounds[i].calls...)
				}
				rounds = tc.change(rounds)
				if name == "a result is an error once" && w.p != ProtocolClaude {
					t.Skip("only Claude flags a result as an error; the others carry it in the text")
				}
				loop, ok := detect(w, rounds, 8)
				if tc.turns == 0 {
					if ok {
						t.Fatalf("taken for a loop: %+v", loop)
					}
					return
				}
				if !ok || loop.Turns != tc.turns {
					t.Fatalf("turns = %d (%v), want %d", loop.Turns, ok, tc.turns)
				}
			})
		})
	}
}

// A result that answers no call of its turn is part of what the client said.
// One that differs from turn to turn is progress; one that is the same every time
// is part of the loop.
func TestToolLoopReadsAResultOfNoCallAsPartOfTheTurn(t *testing.T) {
	write := func(turns int, stray func(i int) string) []byte {
		msgs := []any{map[string]any{"role": "user", "content": "fix it"}}
		for i := 0; i < turns; i++ {
			id := fmt.Sprintf("toolu_%d", i)
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"file_path": "/a.go"}}}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": id, "content": "package a"},
					map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("toolu_stray_%d", i), "content": stray(i)},
				}},
			)
		}
		return []byte(jsonOf(map[string]any{"messages": msgs}))
	}
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}
	if loop, ok := DetectToolLoop(ProtocolClaude, write(10, func(int) string { return "late result" }), ToolLimits{Repeats: 8}); !ok || loop.Turns != 10 {
		t.Fatalf("turns with the same stray result: %+v %v", loop, ok)
	}
	if loop, ok := DetectToolLoop(ProtocolClaude, write(10, func(i int) string { return "late result " + words[i] }), ToolLimits{Repeats: 8}); ok {
		t.Fatalf("a stray result that differs every turn is progress: %+v", loop)
	}
}

func TestToolLoopLeavesTheOrderOfParallelCallsOut(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		a := toolCall{name: "Read", args: `{"file_path":"/a"}`, out: "AAA"}
		b := toolCall{name: "Grep", args: `{"pattern":"x"}`, out: "BBB"}
		rounds := make([]round, 10)
		for i := range rounds {
			rounds[i] = round{calls: []toolCall{a, b}}
			if i%2 == 1 {
				rounds[i] = round{calls: []toolCall{b, a}}
			}
		}
		if loop, ok := detect(w, rounds, 8); !ok || loop.Turns != 10 || len(loop.Calls) != 2 {
			t.Fatalf("%+v %v", loop, ok)
		}
		// one of two calls getting a new result is progress
		rounds[9].calls[0].out = "different"
		if loop, ok := detect(w, rounds, 8); ok {
			t.Fatalf("%+v", loop)
		}
	})
}

// Two calls to one tool in a turn, answered differently, are told apart by what
// each was asked, even where the API has no ids to pair them (Gemini).
func TestToolLoopPairsResultsWithTheirCalls(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		first := toolCall{name: "Read", args: `{"file_path":"/a"}`, out: "AAA"}
		second := toolCall{name: "Read", args: `{"file_path":"/b"}`, out: "BBB"}
		swapped := []toolCall{
			{name: "Read", args: first.args, out: second.out},
			{name: "Read", args: second.args, out: first.out},
		}
		rounds := same(10, round{calls: []toolCall{first, second}})
		if _, ok := detect(w, rounds, 8); !ok {
			t.Fatal("identical rounds are a loop")
		}
		rounds[9].calls = swapped
		if loop, ok := detect(w, rounds, 8); ok {
			t.Fatalf("results given to the other call are progress: %+v", loop)
		}
	})
}

func TestToolLoopCountsOnlyTheNewestTurns(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		var rounds []round
		// a long history of progress, then 9 identical rounds
		for i := 0; i < 400; i++ {
			rounds = append(rounds, round{calls: []toolCall{{name: "Read", args: fmt.Sprintf(`{"file_path":"/f%d"}`, i), out: fmt.Sprintf("file %d", i)}}})
		}
		rounds = append(rounds, same(9, round{calls: []toolCall{readFile("/loop.go")}})...)
		loop, ok := detect(w, rounds, 8)
		if !ok || loop.Turns != 9 {
			t.Fatalf("%+v %v", loop, ok)
		}
		if loop.Items < 800 || loop.Read > 8*8 {
			t.Errorf("read %d of %d items", loop.Read, loop.Items)
		}
		// a loop long ago, then progress
		rounds = append(same(12, round{calls: []toolCall{readFile("/old.go")}}), rounds[:50]...)
		if loop, ok := detect(w, rounds, 8); ok {
			t.Fatalf("an old loop: %+v", loop)
		}
	})
}

func TestToolLoopNeedsTheConversationToEndOnResults(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		rounds := same(10, round{calls: []toolCall{readFile("/repo/main.go")}})
		body := w.write("p", rounds, true)
		if loop, ok := DetectToolLoop(w.p, body, ToolLimits{Repeats: 8}); ok {
			// the last round has no answer yet: 9 answered rounds remain, and the
			// conversation does not end on their results
			t.Fatalf("a conversation ending on the model's call: %+v", loop)
		}
	})
}

// Alternating between two calls, retrying a failing command that fails in new
// ways, polling something that is moving and reading many files are not loops.
func TestToolLoopLeavesOrdinaryAgentWorkAlone(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		cases := map[string][]round{}
		var alternating, failing, polling, reading, editing []round
		for i := 0; i < 30; i++ {
			if i%2 == 0 {
				alternating = append(alternating, round{calls: []toolCall{readFile("/a.go")}})
			} else {
				alternating = append(alternating, round{calls: []toolCall{{name: "Bash", args: `{"command":"go build"}`, out: "ok"}}})
			}
			failing = append(failing, round{calls: []toolCall{{name: "Bash", args: `{"command":"go test ./..."}`, out: fmt.Sprintf("FAIL: %d tests failed", 30-i), isErr: true}}})
			polling = append(polling, round{calls: []toolCall{{name: "Bash", args: `{"command":"gh run view 1"}`, out: fmt.Sprintf("in progress, step %d of 40", i)}}})
			reading = append(reading, round{calls: []toolCall{readFile(fmt.Sprintf("/repo/f%d.go", i))}})
			// the same test run between different edits: the edit differs each time
			editing = append(editing, round{calls: []toolCall{
				{name: "Edit", args: fmt.Sprintf(`{"file":"a.go","line":%d}`, i), out: "edited"},
				{name: "Bash", args: `{"command":"go test ./..."}`, out: "FAIL: TestX"},
			}})
		}
		cases["alternating"], cases["failing differently"], cases["polling a moving thing"], cases["reading many files"], cases["editing between runs"] =
			alternating, failing, polling, reading, editing
		for name, rounds := range cases {
			if loop, ok := detect(w, rounds, 8); ok {
				t.Errorf("%s: taken for a loop: %+v", name, loop)
			}
		}
	})
}

func TestToolLoopStripsWhatARunWritesAboutItself(t *testing.T) {
	eachProtocol(t, func(t *testing.T, w writer) {
		rounds := make([]round, 10)
		for i := range rounds {
			out := fmt.Sprintf("Chunk ID: %x\nWall time: 0.%04d seconds\nProcess exited with code 1\nOutput:\nerror: cannot find package", 0xabc000+i, 17+i)
			rounds[i] = round{calls: []toolCall{{name: "shell", args: `{"command":["go","build"]}`, out: out}}}
		}
		if loop, ok := detect(w, rounds, 8); !ok || loop.Turns != 10 {
			t.Fatalf("%+v %v", loop, ok)
		}
		rounds[9].calls[0].out = strings.Replace(rounds[9].calls[0].out, "cannot find package", "cannot find module", 1)
		if loop, ok := detect(w, rounds, 8); ok {
			t.Fatalf("a different message is progress: %+v", loop)
		}
		// a shell tool's duration
		for i := range rounds {
			rounds[i].calls[0].out = fmt.Sprintf(`{"exit_code":1,"duration_seconds":%d.%d,"stdout":"no"}`, i, i*7)
		}
		if loop, ok := detect(w, rounds, 8); !ok || loop.Turns != 10 {
			t.Fatalf("%+v %v", loop, ok)
		}
	})
}

func TestToolLoopLimitsAreHeldToTheirBounds(t *testing.T) {
	for in, want := range map[int]int{-3: 8, 0: 8, 1: 3, 2: 3, 3: 3, 8: 8, 32: 32, 33: 32, 1 << 20: 32} {
		if got := (ToolLimits{Repeats: in}).Normalized().Repeats; got != want {
			t.Errorf("Repeats %d -> %d, want %d", in, got, want)
		}
	}
	// a lowered limit finds a shorter loop
	read := round{calls: []toolCall{readFile("/repo/main.go")}}
	if _, ok := DetectToolLoop(ProtocolClaude, writers[0].write("p", same(3, read), false), ToolLimits{Repeats: 3}); !ok {
		t.Error("3 rounds are a loop at a limit of 3")
	}
	if _, ok := DetectToolLoop(ProtocolClaude, writers[0].write("p", same(3, read), false), ToolLimits{Repeats: 1}); !ok {
		t.Error("a limit under the floor is held to the floor, not off")
	}
	if _, ok := DetectToolLoop(ProtocolClaude, writers[0].write("p", same(2, read), false), ToolLimits{Repeats: 1}); ok {
		t.Error("2 rounds are under the floor")
	}
}

func TestToolLoopLetsEverythingElseBy(t *testing.T) {
	for _, body := range []string{
		"", "null", "[]", "{}", "not json", `{"messages":"text"}`, `{"messages":[]}`, `{"messages":[1,2,3]}`,
		`{"messages":[{"role":"assistant"},{"role":"user"}]}`, `{"messages":[null,{"content":5}]}`,
		`{"input":"just a prompt"}`, `{"input":[{"type":"function_call_output"}]}`,
		`{"contents":[{"role":"model","parts":"x"}]}`, `{"contents":{"role":"user"}}`,
		strings.Repeat(`{"tool_result":"tool_call_id"`, 30),
	} {
		for _, p := range []Protocol{ProtocolClaude, ProtocolChat, ProtocolResponses, ProtocolGemini, Protocol("other"), ""} {
			if loop, ok := DetectToolLoop(p, []byte(body), ToolLimits{}); ok {
				t.Errorf("%q as %q: %+v", body, p, loop)
			}
		}
	}
}

func TestProtocolOf(t *testing.T) {
	for in, want := range map[string]Protocol{"openai": ProtocolChat, "openai-response": ProtocolResponses, "claude": ProtocolClaude, "gemini": ProtocolGemini} {
		if got, ok := ProtocolOf(in); !ok || got != want {
			t.Errorf("ProtocolOf(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "gemini-cli", "codex", "OpenAI"} {
		if got, ok := ProtocolOf(in); ok {
			t.Errorf("ProtocolOf(%q) = %q", in, got)
		}
	}
}

func TestToolLoopNoticeNamesTheCalls(t *testing.T) {
	loop := ToolLoop{Turns: 9, Repeats: 8, Calls: []ToolCall{
		{Name: "Read", Arguments: `{"file_path":"/a.go"}`}, {Name: "Grep", Arguments: `{"pattern":"x"}`},
		{Name: "Bash", Arguments: `{"command":"ls"}`}, {Name: "Glob"},
	}}
	notice := loop.Notice()
	for _, want := range []string{"9 turns in a row", `Read {"file_path":"/a.go"}`, `Grep {"pattern":"x"}`, "Bash", "…", "sent nowhere", "Stop tool-call loops", "Loop protection"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q lacks %q", notice, want)
		}
	}
	if strings.Contains(notice, "Glob") {
		t.Errorf("only the first %d calls are named: %q", maxNoticeCalls, notice)
	}
	detail := loop.Detail()
	for _, want := range []string{"tool loop", "9 turns", "threshold 8", "nothing was sent upstream", "Read"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
	// the words of an over-long prompt would make a handler rewrite the answer
	// for a client that compacts its conversation
	for _, word := range []string{"context length", "context window", "too long", "token"} {
		if strings.Contains(strings.ToLower(notice), word) {
			t.Errorf("the notice says %q", word)
		}
	}
}

// Large requests are the point of reading only the tail: a conversation of
// megabytes is scanned, not decoded.
func BenchmarkDetectToolLoopLargeConversation(b *testing.B) {
	for _, w := range writers {
		var rounds []round
		bulk := strings.Repeat("line of a file the tool read, with some words in it\n", 40)
		for i := 0; i < 300; i++ {
			rounds = append(rounds, round{calls: []toolCall{{name: "Read", args: fmt.Sprintf(`{"file_path":"/f%d"}`, i), out: fmt.Sprintf("%d\n%s", i, bulk)}}})
		}
		body := w.write("p", rounds, false)
		b.Run(fmt.Sprintf("%s/%dKB", w.name, len(body)>>10), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := DetectToolLoop(w.p, body, ToolLimits{}); ok {
					b.Fatal("not a loop")
				}
			}
		})
	}
}

func FuzzDetectToolLoop(f *testing.F) {
	for _, w := range writers {
		f.Add(string(w.p), w.write("p", same(9, round{calls: []toolCall{readFile("/a")}}), false))
	}
	f.Add("claude", []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"x","input":5}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":{"a":1}}]}]}`))
	f.Fuzz(func(t *testing.T, protocol string, body []byte) {
		DetectToolLoop(Protocol(protocol), body, ToolLimits{Repeats: 3})
	})
}
