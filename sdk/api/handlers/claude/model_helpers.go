package claude

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
)

// MessagesModel extracts the model selected by an Anthropic Messages request.
func MessagesModel(_ *gin.Context, body []byte) string {
	return strings.TrimSpace(ojson.GetBytesNoCopy(body, "model").String())
}
