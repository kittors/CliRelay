package api

import (
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

// requestBodyMiddleware refuses an inference request whose body is not one
// strict JSON text (bodyutil.CheckJSONRequestBody) before any later stage reads
// it. The stages after it read the model with encoding/json, which takes the
// last of two "model" members; the handler, the request guard and the executors
// read the first with gjson; the upstream reads whichever its parser does. A
// request every one of them reads alike is the only kind that may go on.
//
// It runs before quota admission, so a refused request holds no slot.
func requestBodyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// A WebSocket upgrade has no body: each turn comes in a frame, which
		// the handler checks. A form (the image edit endpoint takes
		// multipart/form-data) is read by its handler, which builds the request
		// it executes from the fields, and checks a JSON body it reads instead
		// (handlers.ReadJSONRequestBody).
		if middleware.IsWebsocketUpgrade(c.Request) || isMultipartForm(c.Request) {
			c.Next()
			return
		}
		if err := bodyutil.CheckJSONRequestBody(c, bodyutil.ModelRequestBodyLimit()); err != nil {
			handlers.WriteRequestBodyError(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}

func isMultipartForm(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "multipart/form-data"
}
