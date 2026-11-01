package loopdetect_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	claudeopenai "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/claude/openai/chat-completions"
	codexclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/codex/claude"
	codexopenai "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/codex/openai/chat-completions"
	geminiclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/claude"
	geminiopenai "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/openai/chat-completions"
	openaiclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	"github.com/tidwall/gjson"
)

const replyText = "Stopped by the gateway's loop protection: the agent ran \"Read\" 9 turns in a row <b>&</b> got 'the same' result.\nSecond line, with 好 and 😀."

func reply(p loopdetect.Protocol) loopdetect.LocalReply {
	return loopdetect.LocalReply{Protocol: p, Model: "claude-sonnet-4", Text: replyText, ID: "loopguard01", Created: 1761955200}
}

var allProtocols = []loopdetect.Protocol{loopdetect.ProtocolChat, loopdetect.ProtocolClaude, loopdetect.ProtocolResponses, loopdetect.ProtocolGemini}

func TestLocalReplyJSONIsAFinishedAssistantMessage(t *testing.T) {
	for _, tc := range []struct {
		p        loopdetect.Protocol
		textPath string
		checks   map[string]string
	}{
		{loopdetect.ProtocolChat, "choices.0.message.content", map[string]string{
			"id": "chatcmpl-loopguard01", "object": "chat.completion", "model": "claude-sonnet-4", "choices.0.finish_reason": "stop",
			"choices.0.message.role": "assistant", "usage.total_tokens": "0",
		}},
		{loopdetect.ProtocolClaude, "content.0.text", map[string]string{
			"id": "msg_loopguard01", "type": "message", "role": "assistant", "model": "claude-sonnet-4", "stop_reason": "end_turn",
			"content.0.type": "text", "usage.output_tokens": "0",
		}},
		{loopdetect.ProtocolResponses, "output.0.content.0.text", map[string]string{
			"id": "resp_loopguard01", "object": "response", "status": "completed", "model": "claude-sonnet-4",
			"output.0.type": "message", "output.0.role": "assistant", "output.0.id": "msg_loopguard01", "output.0.status": "completed",
			"output.0.content.0.type": "output_text", "usage.total_tokens": "0",
		}},
		{loopdetect.ProtocolGemini, "candidates.0.content.parts.0.text", map[string]string{
			"candidates.0.finishReason": "STOP", "candidates.0.content.role": "model", "modelVersion": "claude-sonnet-4",
			"usageMetadata.totalTokenCount": "0",
		}},
	} {
		t.Run(string(tc.p), func(t *testing.T) {
			body := reply(tc.p).JSON()
			if !gjson.ValidBytes(body) {
				t.Fatalf("not JSON: %s", body)
			}
			root := gjson.ParseBytes(body)
			if got := root.Get(tc.textPath).String(); got != replyText {
				t.Errorf("text = %q", got)
			}
			for path, want := range tc.checks {
				if got := root.Get(path).String(); got != want {
					t.Errorf("%s = %q, want %q", path, got, want)
				}
			}
			// nothing to call: a reply that ends the agent's turn
			for _, bad := range []string{"tool_calls", "tool_use", "functionCall", "function_call"} {
				if bytes.Contains(body, []byte(bad)) {
					t.Errorf("the reply has %s in it", bad)
				}
			}
		})
	}
	if got := (loopdetect.LocalReply{Protocol: "other"}).JSON(); got != nil {
		t.Errorf("an unknown protocol: %s", got)
	}
	if got := (loopdetect.LocalReply{Protocol: "other"}).Stream(); got != nil {
		t.Errorf("an unknown protocol: %q", got)
	}
}

// sseData is the data line of an event, which is what the executors hand to the
// translators of the other direction.
func sseData(chunk []byte) []byte {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(line)
		}
	}
	return nil
}

func TestLocalReplyStreamIsWellFormed(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		chunks := reply(loopdetect.ProtocolClaude).Stream()
		var types []string
		var text strings.Builder
		for _, c := range chunks {
			s := string(c)
			if !strings.HasPrefix(s, "event: ") || !strings.HasSuffix(s, "\n\n") {
				t.Fatalf("an event is `event:` and `data:` lines closed by a blank line: %q", s)
			}
			data := gjson.Parse(string(sseData(c))[len("data:"):])
			ev := strings.TrimPrefix(strings.SplitN(s, "\n", 2)[0], "event: ")
			if data.Get("type").String() != ev {
				t.Errorf("event %q has type %q", ev, data.Get("type").String())
			}
			types = append(types, ev)
			text.WriteString(data.Get("delta.text").String())
		}
		want := "message_start content_block_start content_block_delta content_block_stop message_delta message_stop"
		if strings.Join(types, " ") != want {
			t.Errorf("events %v", types)
		}
		if text.String() != replyText {
			t.Errorf("text %q", text.String())
		}
		if gjson.GetBytes(sseData(chunks[0])[len("data:"):], "message.id").String() != "msg_loopguard01" {
			t.Errorf("message_start: %s", chunks[0])
		}
		if !strings.Contains(string(chunks[4]), `"stop_reason":"end_turn"`) {
			t.Errorf("message_delta: %s", chunks[4])
		}
	})
	t.Run("responses", func(t *testing.T) {
		chunks := reply(loopdetect.ProtocolResponses).Stream()
		var types []string
		for i, c := range chunks {
			s := string(c)
			// the handler adds the blank lines; a chunk is two lines
			if !strings.HasPrefix(s, "event: ") || strings.HasSuffix(s, "\n") || strings.Count(s, "\n") != 1 {
				t.Fatalf("a chunk is an `event:` line and a `data:` line, with no blank line: %q", s)
			}
			data := gjson.Parse(string(sseData(c))[len("data:"):])
			ev := strings.TrimPrefix(strings.SplitN(s, "\n", 2)[0], "event: ")
			if data.Get("type").String() != ev || data.Get("sequence_number").Int() != int64(i) {
				t.Errorf("event %d: %s", i, s)
			}
			types = append(types, ev)
		}
		want := "response.created response.in_progress response.output_item.added response.content_part.added response.output_text.delta " +
			"response.output_text.done response.content_part.done response.output_item.done response.completed"
		if strings.Join(types, " ") != want {
			t.Errorf("events %v", types)
		}
		completed := gjson.Parse(string(sseData(chunks[len(chunks)-1]))[len("data:"):])
		if completed.Get("response.output.0.content.0.text").String() != replyText || completed.Get("response.status").String() != "completed" {
			t.Errorf("response.completed: %s", chunks[len(chunks)-1])
		}
		if gjson.Parse(string(sseData(chunks[0]))[len("data:"):]).Get("response.status").String() != "in_progress" {
			t.Errorf("response.created: %s", chunks[0])
		}
	})
	t.Run("chat", func(t *testing.T) {
		chunks := reply(loopdetect.ProtocolChat).Stream()
		if len(chunks) != 3 {
			t.Fatalf("%d chunks", len(chunks))
		}
		var text strings.Builder
		for i, c := range chunks {
			root := gjson.ParseBytes(c)
			if !root.Exists() || root.Get("object").String() != "chat.completion.chunk" || root.Get("id").String() != "chatcmpl-loopguard01" {
				t.Fatalf("chunk %d: %s", i, c)
			}
			text.WriteString(root.Get("choices.0.delta.content").String())
		}
		if text.String() != replyText {
			t.Errorf("text %q", text.String())
		}
		if gjson.GetBytes(chunks[0], "choices.0.delta.role").String() != "assistant" ||
			gjson.GetBytes(chunks[2], "choices.0.finish_reason").String() != "stop" ||
			gjson.GetBytes(chunks[1], "choices.0.finish_reason").Type != gjson.Null {
			t.Errorf("roles and finish reasons: %s", chunks)
		}
	})
	t.Run("gemini", func(t *testing.T) {
		chunks := reply(loopdetect.ProtocolGemini).Stream()
		if len(chunks) != 1 {
			t.Fatalf("%d chunks", len(chunks))
		}
		root := gjson.ParseBytes(chunks[0])
		if root.Get("candidates.0.content.parts.0.text").String() != replyText || root.Get("candidates.0.finishReason").String() != "STOP" {
			t.Errorf("%s", chunks[0])
		}
	})
}

// A client reads the stream with the parsers of its own API. The gateway's
// translators read the same events when an upstream speaks them, so each stream
// goes through the translator that takes it as an upstream's, which must find
// the text and the end in it.
func TestLocalReplyStreamIsReadByTheTranslatorsOfEachAPI(t *testing.T) {
	ctx := context.Background()
	// run feeds each chunk as the executors do, a `data:` line, and returns the
	// translator's output.
	run := func(chunks [][]byte, bare bool, convert func(ctx context.Context, model string, orig, req, raw []byte, param *any) []string, orig string, done string) string {
		var param any
		var out strings.Builder
		for _, c := range chunks {
			line := sseData(c)
			if bare || line == nil {
				line = append([]byte("data: "), c...)
			}
			for _, s := range convert(ctx, "claude-sonnet-4", []byte(orig), []byte(orig), line, &param) {
				out.WriteString(s)
				out.WriteString("\n")
			}
		}
		if done != "" {
			for _, s := range convert(ctx, "claude-sonnet-4", []byte(orig), []byte(orig), []byte(done), &param) {
				out.WriteString(s)
				out.WriteString("\n")
			}
		}
		return out.String()
	}
	// textOf joins every text the translated events carry, in a Chat Completions
	// chunk or a Claude event.
	textOf := func(translated string) (text string, finish string, stopped bool) {
		var b strings.Builder
		for _, block := range strings.Split(translated, "\n") {
			block = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(block), "data:"))
			if !gjson.Valid(block) || !strings.HasPrefix(block, "{") {
				continue
			}
			root := gjson.Parse(block)
			b.WriteString(root.Get("choices.0.delta.content").String())
			b.WriteString(root.Get("delta.text").String())
			if f := root.Get("choices.0.finish_reason"); f.Type == gjson.String {
				finish = f.String()
			}
			if root.Get("type").String() == "message_stop" {
				stopped = true
			}
		}
		return b.String(), finish, stopped
	}

	t.Run("claude stream as an upstream of a Chat client", func(t *testing.T) {
		out := run(reply(loopdetect.ProtocolClaude).Stream(), false, claudeopenai.ConvertClaudeResponseToOpenAI, `{"stream":true}`, "")
		text, finish, _ := textOf(out)
		if text != replyText || finish != "stop" {
			t.Fatalf("text %q finish %q\n%s", text, finish, out)
		}
	})
	t.Run("responses stream as an upstream of a Chat client", func(t *testing.T) {
		out := run(reply(loopdetect.ProtocolResponses).Stream(), false, codexopenai.ConvertCodexResponseToOpenAI, `{"stream":true}`, "")
		text, finish, _ := textOf(out)
		if text != replyText || finish != "stop" {
			t.Fatalf("text %q finish %q\n%s", text, finish, out)
		}
	})
	t.Run("responses stream as an upstream of a Claude client", func(t *testing.T) {
		out := run(reply(loopdetect.ProtocolResponses).Stream(), false, codexclaude.ConvertCodexResponseToClaude, `{"stream":true}`, "")
		text, _, stopped := textOf(out)
		if text != replyText || !stopped {
			t.Fatalf("text %q stopped %v\n%s", text, stopped, out)
		}
	})
	t.Run("chat stream as an upstream of a Claude client", func(t *testing.T) {
		out := run(reply(loopdetect.ProtocolChat).Stream(), true, openaiclaude.ConvertOpenAIResponseToClaude, `{"stream":true}`, "data: [DONE]")
		text, _, stopped := textOf(out)
		if text != replyText || !stopped {
			t.Fatalf("text %q stopped %v\n%s", text, stopped, out)
		}
	})
	t.Run("gemini stream as an upstream of a Chat client", func(t *testing.T) {
		out := run(reply(loopdetect.ProtocolGemini).Stream(), true, geminiopenai.ConvertGeminiResponseToOpenAI, `{"stream":true}`, "")
		text, finish, _ := textOf(out)
		if text != replyText || finish != "stop" {
			t.Fatalf("text %q finish %q\n%s", text, finish, out)
		}
	})
	t.Run("gemini stream as an upstream of a Claude client", func(t *testing.T) {
		out := run(reply(loopdetect.ProtocolGemini).Stream(), true, geminiclaude.ConvertGeminiResponseToClaude, `{"stream":true}`, "[DONE]")
		text, _, stopped := textOf(out)
		if text != replyText || !stopped {
			t.Fatalf("text %q stopped %v\n%s", text, stopped, out)
		}
	})
}
