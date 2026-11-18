package management

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authevents"
)

// exportPageSize is the database page size used while streaming. Large enough to
// keep the query count sane, small enough that one page never sits in memory as
// a meaningful cost.
const exportPageSize = 500

// maxExportRows bounds one export. An unbounded export over an attack window is
// a way to make the server build a hundred-megabyte response for a single click.
const maxExportRows = 50000

// GetAuthAttemptsExport streams matching attempts as CSV.
//
// Streaming rather than building the file in memory: the rows worth exporting
// are exactly the ones produced during an attack, which is when the process can
// least afford to buffer all of them.
func (h *Handler) GetAuthAttemptsExport(c *gin.Context) {
	recorder := authevents.Default()
	if recorder == nil || !recorder.Available() {
		authEventsUnavailable(c)
		return
	}
	filter := authevents.ListFilter{
		IPPrefix: strings.TrimSpace(c.Query("ip")),
		Username: strings.TrimSpace(c.Query("username")),
		Outcome:  strings.TrimSpace(c.Query("outcome")),
		Surface:  strings.TrimSpace(c.Query("surface")),
		Size:     exportPageSize,
	}
	if window, ok := summaryWindows[strings.TrimSpace(c.Query("window"))]; ok {
		filter.Since = time.Now().Add(-window)
	}

	filename := fmt.Sprintf("auth-attempts-%s.csv", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)

	writer := csv.NewWriter(c.Writer)
	// A BOM so Excel opens UTF-8 correctly; without it Chinese reasons and
	// usernames arrive as mojibake, which is the main way this file gets read.
	_, _ = c.Writer.Write([]byte{0xEF, 0xBB, 0xBF})
	if err := writer.Write([]string{
		"occurred_at", "ip", "ip_prefix", "trusted", "scope", "surface",
		"username", "outcome", "reason", "user_agent", "request_path", "request_id",
	}); err != nil {
		return
	}

	written := 0
	for page := 1; written < maxExportRows; page++ {
		filter.Page = page
		events, total, err := recorder.List(c.Request.Context(), filter)
		if err != nil {
			// Headers are already sent, so the only honest signal left is to stop
			// writing; the partial file ends at the last complete row.
			writer.Flush()
			return
		}
		if len(events) == 0 {
			break
		}
		for i := range events {
			event := events[i]
			if err = writer.Write([]string{
				event.OccurredAt.Format(time.RFC3339),
				event.IP,
				event.IPPrefix,
				strconv.FormatBool(event.Trusted),
				event.Scope,
				event.Surface,
				event.Username,
				event.Outcome,
				event.Reason,
				event.UserAgent,
				event.RequestPath,
				event.RequestID,
			}); err != nil {
				writer.Flush()
				return
			}
			written++
			if written >= maxExportRows {
				break
			}
		}
		writer.Flush()
		if c.Writer.Status() >= http.StatusBadRequest {
			return
		}
		if page*filter.Size >= total {
			break
		}
	}
	writer.Flush()
}
