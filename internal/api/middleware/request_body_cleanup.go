package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// RequestBodyCleanupMiddleware releases reusable request-body and API exchange
// storage after request logging and handler processing. Defers also cover panic
// unwinding before the outer recovery middleware writes its response.
func RequestBodyCleanupMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer bodyutil.CleanupRequestBody(c)
		defer logging.CleanupAPIExchange(c)
		c.Next()
	}
}
