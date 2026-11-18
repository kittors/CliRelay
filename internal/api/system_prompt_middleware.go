package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func SystemPromptMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Alpha Search commands must not acquire chat-only fields.
		if c.Request.Method != http.MethodPost || strings.HasSuffix(c.FullPath(), "/alpha/search") {
			c.Next()
			return
		}

		metadataVal, exists := c.Get("accessMetadata")
		if !exists {
			c.Next()
			return
		}
		metadata, ok := metadataVal.(map[string]string)
		if !ok {
			c.Next()
			return
		}
		systemPrompt, exists := metadata["system-prompt"]
		if !exists || strings.TrimSpace(systemPrompt) == "" {
			c.Next()
			return
		}

		bodyBytes, err := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
				return
			}
			c.Next()
			return
		}

		var newBody []byte
		if gjson.GetBytes(bodyBytes, "messages").Exists() && gjson.GetBytes(bodyBytes, "messages").IsArray() {
			sysMsg := map[string]interface{}{
				"role":    "system",
				"content": systemPrompt,
			}
			var body map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				bodyutil.SetRequestBody(c, bodyBytes)
				c.Next()
				return
			}
			messages, _ := body["messages"].([]interface{})
			newMessages := make([]interface{}, 0, len(messages)+1)
			newMessages = append(newMessages, sysMsg)
			newMessages = append(newMessages, messages...)
			body["messages"] = newMessages
			newBody, err = json.Marshal(body)
			if err != nil {
				bodyutil.SetRequestBody(c, bodyBytes)
				c.Next()
				return
			}
			log.Debugf("[SystemPrompt] injected into messages (count: %d→%d)", len(messages), len(newMessages))
		} else if gjson.GetBytes(bodyBytes, "input").Exists() {
			existing := strings.TrimSpace(gjson.GetBytes(bodyBytes, "instructions").String())
			combined := systemPrompt
			if existing != "" {
				combined = systemPrompt + "\n\n" + existing
			}
			newBody, _ = sjson.SetBytes(bodyBytes, "instructions", combined)
			if newBody == nil {
				bodyutil.SetRequestBody(c, bodyBytes)
				c.Next()
				return
			}
			log.Debugf("[SystemPrompt] injected into instructions (Responses API)")
		} else {
			bodyutil.SetRequestBody(c, bodyBytes)
			c.Next()
			return
		}

		bodyutil.SetRequestBody(c, newBody)
		c.Next()
	}
}
