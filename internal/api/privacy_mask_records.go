package api

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// privacyMaskRecordMiddleware writes the privacy masking record of a request
// once it is over: a streamed reply has been put back and counted by then, and
// a request the guard held back has a record too, though it has no request log
// row. The record is queued for a writer of its own; nothing here waits for
// the database.
func privacyMaskRecordMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		snapshot, ok := contentmoderation.SecretMaskingSnapshotFromGin(c)
		if !ok {
			return
		}
		record := snapshot.Record(contentmoderation.MaskClient(c.Request.UserAgent()))
		if record == nil {
			return
		}
		if !usage.RequestLogBodyStorageEnabled() {
			// the snippets are text of the request: kept only while bodies are
			record.Snippets = nil
		}
		contentmoderation.MaskRecords().Add(record)
	}
}
