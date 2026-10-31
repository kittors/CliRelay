package executor

import (
	"bytes"
	"math"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// maxClaudeBlockIndex is the largest integer exactly representable as a JSON number.
const maxClaudeBlockIndex = 9007199254740991

var claudeStreamDataTag = []byte("data:")

// claudeContentBlockStop reports whether a raw Claude SSE line is a
// content_block_stop event and, if so, whether its index is a valid
// non-negative integer.
func claudeContentBlockStop(line []byte) (index int64, isStop, valid bool) {
	if !bytes.HasPrefix(line, claudeStreamDataTag) {
		return 0, false, false
	}
	root := gjson.ParseBytes(bytes.TrimSpace(line[len(claudeStreamDataTag):]))
	if root.Get("type").String() != "content_block_stop" {
		return 0, false, false
	}
	indexValue := root.Get("index")
	f := indexValue.Float()
	if indexValue.Type != gjson.Number || f < 0 || f > maxClaudeBlockIndex || math.Trunc(f) != f {
		return 0, true, false
	}
	return indexValue.Int(), true, true
}

// appendClaudeCompletedToolBlockMarkers appends a response-only completion
// marker after the translated OpenAI chunk that carries the complete
// arguments of the tool call stopped at index. Chunks are otherwise unchanged.
// A stop for a block that produced no complete tool call (for example a text
// block or a duplicate stop) adds nothing.
func appendClaudeCompletedToolBlockMarkers(chunks []string, index int64) []string {
	for i := 0; i < len(chunks); i++ {
		call := gjson.Get(chunks[i], "choices.0.delta.tool_calls.0")
		if !call.Exists() || call.Get("index").Int() != index || !call.Get("function.arguments").Exists() {
			continue
		}
		marker, err := sjson.Set(chunks[i], "choices.0.delta", map[string]any{
			"tool_calls": []map[string]any{{"index": index, "id": call.Get("id").String(), "x_tool_completed": sdktranslator.CompletedToolBlocksVersion}},
		})
		if err != nil {
			continue
		}
		chunks = append(chunks[:i+1], append([]string{marker}, chunks[i+1:]...)...)
		i++
	}
	return chunks
}

// translateClaudeLineWithCompletedToolBlocks translates one raw Claude SSE line
// for a client that negotiated completed tool-block markers. A
// content_block_stop with a malformed index is dropped without reaching the
// translator, so it can neither emit nor authorize a completed tool call.
func translateClaudeLineWithCompletedToolBlocks(line []byte, translate func([]byte) []string) []string {
	index, isStop, valid := claudeContentBlockStop(line)
	if isStop && !valid {
		return nil
	}
	chunks := translate(line)
	if isStop {
		chunks = appendClaudeCompletedToolBlockMarkers(chunks, index)
	}
	return chunks
}
