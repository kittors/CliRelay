package executor

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	randSource      = rand.New(rand.NewSource(time.Now().UnixNano()))
	randSourceMutex sync.Mutex
)

func geminiToAntigravity(modelName string, payload []byte, projectID string, sessionGeneration int) []byte {
	var requestRaw string
	if reqNode := gjson.GetBytes(payload, "request"); reqNode.IsObject() {
		requestRaw = reqNode.Raw
	} else {
		requestRaw = string(payload)
	}

	pid := projectID
	if pid == "" {
		pid = generateProjectID()
	}

	template := `{"project":"","model":"","userAgent":"antigravity","requestType":"agent","requestId":"","request":{}}`
	template, _ = sjson.Set(template, "project", pid)
	template, _ = sjson.Set(template, "model", modelName)
	template, _ = sjson.Set(template, "requestId", generateRequestID())
	template, _ = sjson.SetRaw(template, "request", requestRaw)
	template, _ = sjson.Set(template, "request.sessionId", generateStableSessionID(payload, sessionGeneration))

	template, _ = sjson.Delete(template, "request.safetySettings")
	if toolConfig := gjson.Get(template, "toolConfig"); toolConfig.Exists() && !gjson.Get(template, "request.toolConfig").Exists() {
		template, _ = sjson.SetRaw(template, "request.toolConfig", toolConfig.Raw)
		template, _ = sjson.Delete(template, "toolConfig")
	}
	return []byte(template)
}

func generateRequestID() string {
	return "agent-" + uuid.NewString()
}

func generateSessionID() string {
	randSourceMutex.Lock()
	n := randSource.Int63n(9_000_000_000_000_000_000)
	randSourceMutex.Unlock()
	return "-" + strconv.FormatInt(n, 10)
}

// generateStableSessionID derives a deterministic session id from the first user
// turn so that every request in a conversation reuses one upstream session and
// benefits from server-side prefix caching.
//
// sessionGeneration lets the caller rotate that id without changing the
// conversation. The upstream accumulates input server-side per session; once a
// long tool-loop pushes the accumulated input past the 1,048,576-token ceiling,
// every further request on that session fails with a 400 "input token count
// exceeds the maximum". Bumping the generation derives a fresh session id from
// the same first turn, so the conversation transparently continues on a new
// upstream session. Generation 0 is unchanged for backward compatibility.
func generateStableSessionID(payload []byte, sessionGeneration int) string {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.Exists() {
		contents = gjson.GetBytes(payload, "contents")
	}
	if contents.IsArray() {
		for _, content := range contents.Array() {
			if content.Get("role").String() == "user" {
				text := content.Get("parts.0.text").String()
				if text != "" {
					seed := text
					if sessionGeneration > 0 {
						seed = text + "#clirelay-session-gen=" + strconv.Itoa(sessionGeneration)
					}
					h := sha256.Sum256([]byte(seed))
					n := int64(binary.BigEndian.Uint64(h[:8])) & 0x7FFFFFFFFFFFFFFF
					return "-" + strconv.FormatInt(n, 10)
				}
			}
		}
	}
	return generateSessionID()
}

func generateProjectID() string {
	adjectives := []string{"useful", "bright", "swift", "calm", "bold"}
	nouns := []string{"fuze", "wave", "spark", "flow", "core"}
	randSourceMutex.Lock()
	adj := adjectives[randSource.Intn(len(adjectives))]
	noun := nouns[randSource.Intn(len(nouns))]
	randSourceMutex.Unlock()
	randomPart := strings.ToLower(uuid.NewString())[:5]
	return adj + "-" + noun + "-" + randomPart
}
