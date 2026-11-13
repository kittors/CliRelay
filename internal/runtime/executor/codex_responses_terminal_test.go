package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestValidCodexResponsesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          bool
	}{
		{"completed", `{"type":"response.completed","response":{"status":"completed"}}`, true},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, true},
		{"compatible_missing_status", `{"type":"response.completed","response":{}}`, true},
		{"malformed_JSON", `{"type":"response.completed","response":{}`, false},
		{"bare_completed", `{"type":"response.completed"}`, false},
		{"bare_incomplete", `{"type":"response.incomplete"}`, false},
		{"null_response", `{"type":"response.completed","response":null}`, false},
		{"array_response", `{"type":"response.incomplete","response":[]}`, false},
		{"root_array", `[{"type":"response.completed","response":{}}]`, false},
		{"failed", `{"type":"response.failed","response":{}}`, false},
		{"nonstring_type", `{"type":true,"response":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validCodexResponsesTerminal([]byte(tc.payload)); got != tc.want {
				t.Errorf("valid=%t want=%t", got, tc.want)
			}
		})
	}
}

func codexTerminalJSON(kind string, tokens bool) string {
	usage := `"input_tokens":0,"output_tokens":0,"total_tokens":0`
	if tokens {
		usage = `"input_tokens":3,"output_tokens":2,"total_tokens":5`
	}
	return fmt.Sprintf(`{"type":"response.%s","response":{"status":%q,"output":[],"usage":{%s}}}`, kind, kind, usage)
}

// #1185 supplied the terminal-close regression. Cover incomplete and actual
// accounting too: cancellation-aware sends cannot deliver a post-cancel error.
func TestCodexTerminalUsageAndLateErrors(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete"} {
		for _, scenario := range []string{"EOF", "read_error", "duplicate", "duplicate_zero", "duplicate_changed", "late_error", "cancel_on_late_error", "cancel_before_late_error_data"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				terminal := codexTerminalJSON(kind, true)
				body := "data: " + terminal + "\n\n"
				if scenario == "duplicate" {
					body += body
				}
				if scenario == "duplicate_zero" {
					body += "data: " + codexTerminalJSON(kind, false) + "\n\n"
				}
				if scenario == "duplicate_changed" {
					body += "data: " + strings.ReplaceAll(terminal, "3", "9") + "\n\n"
				}
				if strings.Contains(scenario, "late_error") {
					body += "event: error\ndata: " + `{"type":"error","code":"internal_server_error","message":"late provider refusal"}` + "\n\n"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario == "read_error" {
						w.Header().Set("Content-Length", strconv.Itoa(len(body)+64))
					}
					_, _ = w.Write([]byte(body))
				}))
				defer server.Close()
				p := &usageCapturePlugin{records: make(chan coreusage.Record, 32)}
				coreusage.RegisterPlugin(p)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				req, opts := codexTerminalRequest()
				stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalAuth(t.Name(), server.URL), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var chunkErr error
				for chunk := range stream.Chunks {
					if (scenario == "cancel_on_late_error" && strings.Contains(string(chunk.Payload), "late provider refusal")) ||
						(scenario == "cancel_before_late_error_data" && strings.Contains(string(chunk.Payload), "event: error")) {
						cancel()
					}
					if chunk.Err != nil {
						chunkErr = chunk.Err
					}
				}
				failed := strings.Contains(scenario, "late_error")
				if !strings.HasPrefix(scenario, "cancel_") && (chunkErr != nil) != failed {
					t.Errorf("chunk error=%v want failed=%v", chunkErr, failed)
				}
				records := codexTerminalRecords(t, p, t.Name())
				if len(records) != 1 {
					t.Fatalf("records=%d want 1", len(records))
				}
				record := records[0]
				if record.Failed != failed || record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 || record.Detail.TotalTokens != 5 {
					t.Errorf("record failed=%t detail=%+v", record.Failed, record.Detail)
				}
			})
		}
	}
}

func TestCodexTerminalMultilineAndNonStreamError(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete"} {
		for _, mode := range []string{"stream", "nonstream"} {
			for _, lateError := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/error=%t", kind, mode, lateError), func(t *testing.T) {
					body := fmt.Sprintf("event: response.%s\ndata: {\"type\":\"response.%s\",\ndata: \"response\":{\"status\":%q,\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n", kind, kind, kind)
					if lateError {
						body += "event: error\ndata: " + `{"type":"error","code":"internal_server_error","message":"late provider refusal"}` + "\n\n"
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte(body))
					}))
					defer server.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					req, opts := codexTerminalRequest()
					executor := NewCodexExecutor(&config.Config{})
					p := &usageCapturePlugin{records: make(chan coreusage.Record, 32)}
					coreusage.RegisterPlugin(p)
					auth := codexTerminalAuth(t.Name(), server.URL)
					var resultErr error
					if mode == "stream" {
						stream, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								resultErr = chunk.Err
							}
						}
					} else {
						result, err := executor.Execute(ctx, auth, req, opts)
						resultErr = err
						if err == nil && gjson.GetBytes(result.Payload, "status").String() != kind {
							t.Errorf("response=%s", result.Payload)
						}
					}
					if (resultErr != nil) != lateError {
						t.Errorf("error=%v want error=%t", resultErr, lateError)
					}
					records := codexTerminalRecords(t, p, t.Name())
					if len(records) != 1 || records[0].Failed != lateError || records[0].Detail.TotalTokens != 5 {
						t.Errorf("multiline %s records=%+v", mode, records)
					}
				})
			}
		}
	}
}

func TestCodexTerminalNonStreamIgnoresLaterOutputItems(t *testing.T) {
	body := "data: " + codexTerminalJSON("completed", true) + "\n\ndata: " + `{"type":"response.output_item.done","item":{"id":"late","type":"message","role":"assistant","content":[{"type":"output_text","text":"must not join finished response"}]}}` + "\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	req, opts := codexTerminalRequest()
	result, err := NewCodexExecutor(&config.Config{}).Execute(context.Background(), codexTerminalAuth(t.Name(), server.URL), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(result.Payload, "output.#").Int() != 0 {
		t.Errorf("late item joined terminal response: %s", result.Payload)
	}
}

func TestCodexTerminalEarlyCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			p := &usageCapturePlugin{records: make(chan coreusage.Record, 32)}
			coreusage.RegisterPlugin(p)
			req, opts := codexTerminalRequest()
			stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalAuth(t.Name(), server.URL), req, opts)
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range stream.Chunks {
				if len(chunk.Payload) > 0 && !deadline {
					cancel()
				}
			}
			records := codexTerminalRecords(t, p, t.Name())
			if deadline {
				if len(records) != 1 || !records[0].Failed {
					t.Errorf("early deadline records=%+v", records)
				}
			} else if len(records) != 0 {
				t.Errorf("client cancellation recorded account failure: %+v", records)
			}
		})
	}
}

func TestCodexTerminalLoopGuardStillWins(t *testing.T) {
	p := &usageCapturePlugin{records: make(chan coreusage.Record, 32)}
	coreusage.RegisterPlugin(p)
	ctx := context.Background()
	reporter := newUsageReporter(ctx, "codex", "gpt-5.4", "", codexTerminalAuth(t.Name(), "http://unused"), true)
	reporter.guardKind = "reply_loop"
	terminal := codexResponsesTerminal{}
	terminal.observe([]byte(codexTerminalJSON("completed", true)))
	terminal.publishSuccess(ctx, reporter)
	terminal.publishSuccess(ctx, reporter)
	records := codexTerminalRecords(t, p, t.Name())
	if len(records) != 1 || !records[0].Failed || records[0].GuardKind != "reply_loop" || records[0].Detail.TotalTokens != 5 {
		t.Errorf("loop guard record=%+v", records)
	}
}

// Scanner limits are local protocol/resource failures, not transport cleanup.
// Unlike unexpected EOF or cancellation, a valid terminal cannot hide them.
func TestCodexTerminalScannerLimitRemainsFailure(t *testing.T) {
	oversized := strings.Repeat("x", maxCodexSSELineBytes+1)
	for _, kind := range []string{"completed", "incomplete", "missing"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if kind != "missing" {
					_, _ = fmt.Fprint(w, "data: "+codexTerminalJSON(kind, true)+"\n\n")
				}
				_, _ = fmt.Fprint(w, oversized)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
			if !errors.Is(streamErr, bufio.ErrTooLong) {
				t.Errorf("error=%v want scanner limit", streamErr)
			}
			records := codexTerminalRecords(t, p, t.Name())
			if len(records) != 1 || !records[0].Failed {
				t.Fatalf("records=%+v", records)
			}
			wantTokens := int64(5)
			if kind == "missing" {
				wantTokens = 0
			}
			if records[0].Detail.TotalTokens != wantTokens {
				t.Errorf("detail=%+v", records[0].Detail)
			}
		})
	}
}
