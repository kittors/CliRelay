package executor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCollectCodexOutputItemsInjectsCompletionOutput(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[]}}`,
		``,
	}, "\n")

	got := collectCodexOutputItems([]byte(input))
	completed := findSSEPayloadByType(got, "response.completed")
	if len(completed) == 0 {
		t.Fatalf("expected response.completed event after collect")
	}
	if gjson.GetBytes(completed, "response.output.0.id").String() != "msg_1" {
		t.Fatalf("response.output[0].id mismatch, got %q", gjson.GetBytes(completed, "response.output.0.id").String())
	}
	if gjson.GetBytes(completed, "response.output.0.content.0.text").String() != "hello" {
		t.Fatalf("response.output[0].content[0].text mismatch, got %q", gjson.GetBytes(completed, "response.output.0.content.0.text").String())
	}
}

func TestCollectCodexOutputItemsNormalizesResponseDone(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
		``,
		`data: {"type":"response.done","response":{"id":"resp_2","output":[]}}`,
		``,
	}, "\n")

	got := collectCodexOutputItems([]byte(input))
	if bytes.Contains(got, []byte(`"type":"response.done"`)) {
		t.Fatalf("expected response.done to be normalized, got %s", string(got))
	}
	completed := findSSEPayloadByType(got, "response.completed")
	if len(completed) == 0 {
		t.Fatalf("expected normalized response.completed event")
	}
	if gjson.GetBytes(completed, "response.output.0.id").String() != "msg_1" {
		t.Fatalf("expected injected output item, got %q", gjson.GetBytes(completed, "response.output.0.id").String())
	}
}

func TestCollectCodexOutputItemsKeepsExistingOutput(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_3","output":[{"id":"existing_1","type":"message"}]}}`,
		``,
	}, "\n")

	got := collectCodexOutputItems([]byte(input))
	completed := findSSEPayloadByType(got, "response.completed")
	if len(completed) == 0 {
		t.Fatalf("expected response.completed event")
	}
	if gotID := gjson.GetBytes(completed, "response.output.0.id").String(); gotID != "existing_1" {
		t.Fatalf("existing output should be preserved, got %q", gotID)
	}
	if gjson.GetBytes(completed, "response.output.1").Exists() {
		t.Fatalf("unexpected extra output item: %s", gjson.GetBytes(completed, "response.output").Raw)
	}
}

func findSSEPayloadByType(data []byte, eventType string) []byte {
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		payload := bytes.TrimSpace(line[len(dataTag):])
		if gjson.GetBytes(payload, "type").String() == eventType {
			return payload
		}
	}
	return nil
}
