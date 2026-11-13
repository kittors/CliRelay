package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// The Responses adapter emits whole frames. Native Codex still emits lines, so
// use its metadata send as a deterministic barrier after error inspection but
// before the error data can be delivered. Cancellation must not hide that error.
func TestCombinedNativeTerminalErrorCancellationBarrier(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			wire := "data: " + codexTerminalJSON(kind, true) + "\n\nevent: error\ndata: " + `{"type":"error","code":"internal_server_error","message":"error-not-yet-delivered"}` + "\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, wire)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			p := &usageCapturePlugin{records: make(chan coreusage.Record, 32)}
			coreusage.RegisterPlugin(p)
			req, opts := codexTerminalRequest()
			req.Format = sdktranslator.FromString("codex")
			opts.SourceFormat = req.Format
			stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalAuth(t.Name(), server.URL), req, opts)
			if err != nil {
				t.Fatal(err)
			}
			barrier := false
			for chunk := range stream.Chunks {
				if strings.Contains(string(chunk.Payload), "event: error") {
					barrier = true
					cancel()
				}
				if strings.Contains(string(chunk.Payload), "error-not-yet-delivered") {
					t.Error("error data delivered despite cancellation barrier")
				}
			}
			if !barrier {
				t.Fatal("missing error metadata barrier")
			}
			records := codexTerminalRecords(t, p, t.Name())
			if len(records) != 1 || !records[0].Failed || records[0].Detail.TotalTokens != 5 {
				t.Errorf("records=%+v", records)
			}
		})
	}
}

func TestCombinedTerminalFrameAccountingBoundaries(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete"} {
		for _, scenario := range []string{"clean_EOF_unterminated", "read_error_unterminated", "invalid_after_terminal", "two_JSON_values_in_terminal_frame"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				terminal := codexTerminalJSON(kind, true)
				wire := "data: " + terminal
				wantFailure := scenario != "clean_EOF_unterminated"
				wantTokens := int64(5)
				switch scenario {
				case "read_error_unterminated":
					wantTokens = 0
				case "invalid_after_terminal":
					wire += "\n\ndata: not-JSON\n\n"
				case "two_JSON_values_in_terminal_frame":
					wire = "data: {}\ndata: " + terminal + "\n\n"
					wantTokens = 0
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario == "read_error_unterminated" {
						w.Header().Set("Content-Length", fmt.Sprint(len(wire)+64))
					}
					_, _ = fmt.Fprint(w, wire)
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				p := &usageCapturePlugin{records: make(chan coreusage.Record, 32)}
				coreusage.RegisterPlugin(p)
				req, opts := codexTerminalRequest()
				stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalAuth(t.Name(), server.URL), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var streamErr error
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				if (streamErr != nil) != wantFailure {
					t.Errorf("error=%v wantFailure=%t", streamErr, wantFailure)
				}
				records := codexTerminalRecords(t, p, t.Name())
				if len(records) != 1 || records[0].Failed != wantFailure || records[0].Detail.TotalTokens != wantTokens {
					t.Errorf("records=%+v", records)
				}
			})
		}
	}
}
