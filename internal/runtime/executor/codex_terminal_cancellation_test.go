package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestCodexTerminalCancellationRecordsSuccess(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete"} {
		for _, withUsage := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/usage=%t", kind, withUsage), func(t *testing.T) {
				usage := ""
				if withUsage {
					usage = `,"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}`
				}
				payload := fmt.Sprintf(`{"type":"response.%s","response":{"status":%q,"output":[]%s}}`, kind, kind, usage)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: " + payload + "\n\n"))
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer srv.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				p := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 32)}
				cliproxyusage.RegisterPlugin(p)
				req, opts := codexTerminalRequest()
				stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalAuth(t.Name(), srv.URL), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				delivered := false
				for chunk := range stream.Chunks {
					if len(chunk.Payload) > 0 {
						delivered = true
						cancel()
					}
					if chunk.Err != nil {
						t.Errorf("received post-terminal error=%v", chunk.Err)
					}
				}
				if !delivered {
					t.Fatal("terminal never delivered")
				}
				records := codexTerminalRecords(t, p, t.Name())
				var outcomes []bool
				for _, r := range records {
					outcomes = append(outcomes, r.Failed)
				}
				t.Logf("post-cancel usage failures=%v", outcomes)
				if len(records) != 1 || records[0].Failed {
					t.Errorf("post-terminal cancellation must record one success; failures=%v", outcomes)
				}
				if withUsage && len(records) == 1 && records[0].Detail.TotalTokens != 5 {
					t.Errorf("cancelled terminal lost tokens: %+v", records[0].Detail)
				}
			})
		}
	}
}
