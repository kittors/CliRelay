package chat_completions

import (
	"context"
	"strings"
	"testing"
)

func TestConvertOpenAIResponseToOpenAINonStreamUnwrapsProviderEnvelope(t *testing.T) {
	raw := []byte(`{"success":true,"data":{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}}`)

	got := ConvertOpenAIResponseToOpenAINonStream(context.Background(), "qwen3.7-max", nil, nil, raw, nil)

	if strings.Contains(got, `"success"`) || strings.Contains(got, `"data"`) {
		t.Fatalf("response was not unwrapped: %s", got)
	}
	if !strings.Contains(got, `"content":"OK"`) || !strings.Contains(got, `"usage"`) {
		t.Fatalf("unwrapped response missing content or usage: %s", got)
	}
}

func TestConvertOpenAIResponseToOpenAIUnwrapsProviderEnvelopeChunk(t *testing.T) {
	raw := []byte(`data: {"success":true,"data":{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}}`)

	got := ConvertOpenAIResponseToOpenAI(context.Background(), "qwen3.7-max", nil, nil, raw, nil)

	if len(got) != 1 {
		t.Fatalf("chunks len = %d, want 1", len(got))
	}
	if strings.Contains(got[0], `"success"`) || strings.Contains(got[0], `"data"`) {
		t.Fatalf("chunk was not unwrapped: %s", got[0])
	}
	if !strings.Contains(got[0], `"content":"OK"`) {
		t.Fatalf("unwrapped chunk missing content: %s", got[0])
	}
}
