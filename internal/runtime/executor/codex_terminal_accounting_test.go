package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func codexTerminalAuth(id, baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "codex", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": baseURL + "/backend-api/codex"}, Metadata: map[string]any{"access_token": "fixture-token", "account_id": "fixture-account"}}
}
func codexTerminalRequest() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	return cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`), Format: sdktranslator.FromString("openai-response")}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
}
func codexTerminalRecords(t *testing.T, p *usageCapturePlugin, id string) []cliproxyusage.Record {
	t.Helper()
	marker := "terminal-barrier-" + id
	cliproxyusage.PublishRecord(context.Background(), cliproxyusage.Record{Provider: marker})
	var records []cliproxyusage.Record
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case r := <-p.records:
			if r.Provider == marker {
				return records
			}
			if r.AuthID == id {
				records = append(records, r)
			}
		case <-timer.C:
			t.Fatal("usage barrier did not arrive")
			return records
		}
	}
}
func TestCodexTerminalAccounting(t *testing.T) {
	completed := `{"type":"response.completed","response":{"status":"completed","output":[]}}`
	incomplete := `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`
	partial := `{"type":"response.output_text.delta","delta":"partial"}`
	cases := []struct {
		name, body         string
		truncated, wantErr bool
	}{
		{"completed_clean", completed, false, false}, {"incomplete_clean", incomplete, false, false},
		{"completed_transport_error", completed, true, false}, {"incomplete_transport_error", incomplete, true, false},
		{"partial_transport_error", partial, true, true},
		{"malformed_terminal_transport_error", `{"type":"response.completed"}`, true, true},
		{"malformed_terminal_clean", `{"type":"response.completed"}`, false, true},
		{"incomplete_null_response", `{"type":"response.incomplete","response":null}`, false, true},
		{"explicit_error_after_completed", completed + "\n\ndata: " + `{"type":"error","code":"internal_server_error","message":"late terminal error"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: " + tc.body + "\n\n"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.truncated {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)+64))
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			p := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 32)}
			cliproxyusage.RegisterPlugin(p)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, opts := codexTerminalRequest()
			stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalAuth(t.Name(), srv.URL), req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var chunkErr error
			var payloads []string
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					chunkErr = chunk.Err
				}
				if len(chunk.Payload) > 0 {
					payloads = append(payloads, string(chunk.Payload))
				}
			}
			records := codexTerminalRecords(t, p, t.Name())
			var outcomes []bool
			for _, r := range records {
				outcomes = append(outcomes, r.Failed)
			}
			t.Logf("chunk_error=%v; usage_failed=%v; payload_count=%d", chunkErr, outcomes, len(payloads))
			if (chunkErr != nil) != tc.wantErr {
				t.Errorf("error=%v, wantErr=%v", chunkErr, tc.wantErr)
			}
			if len(records) != 1 || records[0].Failed != tc.wantErr {
				t.Errorf("usage failures=%v, want one record with Failed=%v", outcomes, tc.wantErr)
			}
		})
	}
}
func TestCodexTerminalNonStream(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			body := fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":{\"status\":%q,\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n", kind, kind)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			req, opts := codexTerminalRequest()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result, err := NewCodexExecutor(&config.Config{}).Execute(ctx, codexTerminalAuth(t.Name(), srv.URL), req, opts)
			if err != nil {
				t.Fatalf("well-formed response.%s returned error: %v", kind, err)
			}
			if !strings.Contains(string(result.Payload), kind) {
				t.Errorf("terminal status was lost: %s", result.Payload)
			}
		})
	}
}
