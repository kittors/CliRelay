package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestClaudeCompletedToolBlocksPreserveRequestAndReleaseBeforeProviderTerminal(t *testing.T) {
	var bodies [][]byte
	for _, enabled := range []bool{false, true} {
		release := make(chan struct{})
		if !enabled {
			close(release)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if r.Header.Get(sdktranslator.CompletedToolBlocksHeader) != "" {
				t.Error("protocol header leaked to supplier")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []string{
				`{"type":"message_start","message":{"id":"msg_fixture"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_fixture","name":"shell","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"pwd\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
			} {
				_, _ = io.WriteString(w, "data: "+event+"\n\n")
			}
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
		}))
		headers := make(http.Header)
		if enabled {
			headers.Set(sdktranslator.CompletedToolBlocksHeader, sdktranslator.CompletedToolBlocksVersion)
		}
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture-key", "base_url": provider.URL, "cloak_cache_user_id": "true"}}
		payload := []byte(`{"model":"fixture","messages":[{"role":"user","content":"fixture"}],"tools":[{"type":"function","function":{"name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}],"stream":true}`)
		result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "fixture", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true, Headers: headers, OriginalRequest: payload})
		if err != nil {
			cancel()
			provider.Close()
			t.Fatal(err)
		}
		if (result.Headers.Get(sdktranslator.CompletedToolBlocksHeader) == sdktranslator.CompletedToolBlocksVersion) != enabled {
			t.Error("acknowledgment mismatch")
		}
		markerSeen := false
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Error(chunk.Err)
				break
			}
			if strings.Contains(string(chunk.Payload), "x_tool_completed") {
				if !enabled || markerSeen {
					t.Error("unexpected duplicate or unnegotiated marker")
				}
				markerSeen = true
				close(release)
			}
		}
		cancel()
		provider.Close()
		if enabled && !markerSeen {
			t.Fatal("completed block was held until supplier terminal")
		}
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatal("response protocol changed supplier request bytes")
	}
}
