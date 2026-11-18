package openai

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
)

// ChatCompletionsModel extracts the model selected by an OpenAI chat request.
func ChatCompletionsModel(_ *gin.Context, body []byte) string {
	return strings.TrimSpace(ojson.GetBytesNoCopy(body, "model").String())
}
