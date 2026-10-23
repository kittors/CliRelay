package openai

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestForwardResponsesFirstEventDoesNotWaitForNextChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, prefix := range []string{"event: response.output_text.delta\n", ""} {
		for _, suffix := range []string{"", "\n", "\n\n", "\r\n"} {
			t.Run(strings.ReplaceAll(suffix, "\n", "LF"), func(t *testing.T) {
				data := make(chan []byte, 1)
				errs := make(chan *interfaces.ErrorMessage)
				data <- []byte(prefix + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture\"}" + suffix)
				h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					c, _ := gin.CreateTestContext(w)
					c.Request = r
					h.forwardResponsesStream(c, c.Writer, func(error) {}, data, errs)
				}))
				defer server.Close()
				defer close(errs)
				defer close(data)
				client := &http.Client{Timeout: time.Second}
				response, err := client.Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				reader := bufio.NewReader(response.Body)
				seenData := false
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						t.Fatalf("first event waited for another provider chunk: %v", err)
					}
					if strings.HasPrefix(line, "data:") {
						seenData = true
					}
					if seenData && strings.TrimSpace(line) == "" {
						break
					}
				}
			})
		}
	}
}
