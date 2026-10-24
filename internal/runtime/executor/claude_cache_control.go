package executor

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ensureCacheControl injects cache_control breakpoints into the payload for optimal prompt caching.
// According to Anthropic's documentation, cache prefixes are created in order: tools -> system -> messages.
// This function adds cache_control to:
// 1. The LAST tool in the tools array (caches all tool definitions)
// 2. The LAST element in the system array (caches system prompt)
// 3. The SECOND-TO-LAST user turn (caches conversation history for multi-turn)
func ensureCacheControl(payload []byte) []byte {
	payload = injectToolsCacheControl(payload)
	payload = injectSystemCacheControl(payload)
	payload = injectMessagesCacheControl(payload)
	return payload
}

// applyClaudeCacheControl injects cache_control breakpoints unless the client
// already manages caching itself.
//
// The identity fingerprint prepends its own "You are Claude Code" system block
// with a breakpoint. That breakpoint is ours, not the client's, so it must not
// count: with it counted, ensureCacheControl never ran for any request on a
// fingerprinted credential, and tools, the client's system prompt and the whole
// conversation went uncached. The prefix ending at the identity block is far
// below Anthropic's minimum cacheable length, so nothing was cached at all.
func applyClaudeCacheControl(payload []byte) []byte {
	if countClientCacheControls(payload) > 0 {
		return payload
	}
	return ensureCacheControl(payload)
}

// countClientCacheControls counts breakpoints placed by the client, leaving out
// the one the identity fingerprint adds to its own system block.
func countClientCacheControls(payload []byte) int {
	count := countCacheControls(payload)
	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() && isClaudeIdentityBlock(item) {
				count--
			}
			return true
		})
	}
	return count
}

// isClaudeIdentityBlock reports whether a system block is the one the identity
// fingerprint prepends.
func isClaudeIdentityBlock(item gjson.Result) bool {
	return item.Get("text").String() == claudeCodeIdentityText
}

func countCacheControls(payload []byte) int {
	count := 0

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					if item.Get("cache_control").Exists() {
						count++
					}
					return true
				})
			}
			return true
		})
	}

	return count
}

// injectMessagesCacheControl adds cache_control to the second-to-last user turn for multi-turn caching.
func injectMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	hasCacheControlInMessages := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					hasCacheControlInMessages = true
					return false
				}
				return true
			})
		}
		return !hasCacheControlInMessages
	})
	if hasCacheControlInMessages {
		return payload
	}

	var userMsgIndices []int
	messages.ForEach(func(index gjson.Result, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userMsgIndices = append(userMsgIndices, int(index.Int()))
		}
		return true
	})

	if len(userMsgIndices) < 2 {
		return payload
	}

	secondToLastUserIdx := userMsgIndices[len(userMsgIndices)-2]
	contentPath := fmt.Sprintf("messages.%d.content", secondToLastUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		contentCount := int(content.Get("#").Int())
		if contentCount > 0 {
			cacheControlPath := fmt.Sprintf("messages.%d.content.%d.cache_control", secondToLastUserIdx, contentCount-1)
			result, err := sjson.SetBytes(payload, cacheControlPath, map[string]string{"type": "ephemeral"})
			if err != nil {
				log.Warnf("failed to inject cache_control into messages: %v", err)
				return payload
			}
			payload = result
		}
	} else if content.Type == gjson.String {
		text := content.String()
		newContent := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, contentPath, newContent)
		if err != nil {
			log.Warnf("failed to inject cache_control into message string content: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

// injectToolsCacheControl adds cache_control to the last tool in the tools array.
func injectToolsCacheControl(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	toolCount := int(tools.Get("#").Int())
	if toolCount == 0 {
		return payload
	}

	hasCacheControlInTools := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("cache_control").Exists() {
			hasCacheControlInTools = true
			return false
		}
		return true
	})
	if hasCacheControlInTools {
		return payload
	}

	lastToolPath := fmt.Sprintf("tools.%d.cache_control", toolCount-1)
	result, err := sjson.SetBytes(payload, lastToolPath, map[string]string{"type": "ephemeral"})
	if err != nil {
		log.Warnf("failed to inject cache_control into tools array: %v", err)
		return payload
	}

	return result
}

// injectSystemCacheControl adds cache_control to the last element in the system prompt.
func injectSystemCacheControl(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		count := int(system.Get("#").Int())
		if count == 0 {
			return payload
		}

		// The identity fingerprint's own breakpoint ends a prefix far too short
		// to cache, so it does not stand in for one on the client's system prompt.
		hasCacheControlInSystem := false
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() && !isClaudeIdentityBlock(item) {
				hasCacheControlInSystem = true
				return false
			}
			return true
		})
		if hasCacheControlInSystem {
			return payload
		}

		lastSystemPath := fmt.Sprintf("system.%d.cache_control", count-1)
		result, err := sjson.SetBytes(payload, lastSystemPath, map[string]string{"type": "ephemeral"})
		if err != nil {
			log.Warnf("failed to inject cache_control into system array: %v", err)
			return payload
		}
		payload = result
	} else if system.Type == gjson.String {
		text := system.String()
		newSystem := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, "system", newSystem)
		if err != nil {
			log.Warnf("failed to inject cache_control into system string: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}
