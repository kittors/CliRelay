package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestGetLogsReturnsEmptySnapshotWhenFileLoggingDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandler(&config.Config{LoggingToFile: false}, "", nil)
	defer h.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/logs?limit=50000&after=1714567890", nil)

	h.GetLogs(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Lines           []string `json:"lines"`
		LineCount       int      `json:"line-count"`
		LatestTimestamp int64    `json:"latest-timestamp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Lines) != 0 {
		t.Fatalf("lines = %v, want empty", payload.Lines)
	}
	if payload.LineCount != 0 {
		t.Fatalf("line-count = %d, want 0", payload.LineCount)
	}
	if payload.LatestTimestamp != 1714567890 {
		t.Fatalf("latest-timestamp = %d, want 1714567890", payload.LatestTimestamp)
	}
}
