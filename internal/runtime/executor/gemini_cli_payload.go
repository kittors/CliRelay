package executor

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyGeminiCLIHeaders sets required headers for the Gemini CLI upstream.
func applyGeminiCLIHeaders(r *http.Request, cfg *config.Config, auth *cliproxyauth.Auth) {
	if r == nil {
		return
	}
	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	if cfg != nil && cfg.IdentityFingerprint.Gemini.Enabled {
		learned := observeRuntimeIdentityFingerprint(identityfingerprint.ProviderGemini, auth, r.Context())
		fp, _ := identityfingerprint.ResolveGemini(cfg.IdentityFingerprint.Gemini, learned)
		r.Header.Set("User-Agent", fp.UserAgent)
		r.Header.Set("X-Goog-Api-Client", fp.APIClient)
		r.Header.Set("Client-Metadata", fp.ClientMetadata)
		for key, value := range fp.CustomHeaders {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key != "" && value != "" && !isGeminiFingerprintRuntimeBlockedHeader(key) {
				r.Header.Set(key, value)
			}
		}
		return
	}

	misc.EnsureHeader(r.Header, ginHeaders, "User-Agent", config.DefaultGeminiFingerprintUserAgent)
	misc.EnsureHeader(r.Header, ginHeaders, "X-Goog-Api-Client", config.DefaultGeminiFingerprintAPIClient)
	misc.EnsureHeader(r.Header, ginHeaders, "Client-Metadata", geminiCLIClientMetadata())
}

// geminiCLIClientMetadata returns a compact metadata string required by upstream.
func geminiCLIClientMetadata() string {
	// Keep parity with CLI client defaults
	return config.DefaultGeminiFingerprintClientMetadata
}

func isGeminiFingerprintRuntimeBlockedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "connection",
		"user-agent", "x-goog-api-client", "client-metadata":
		return true
	default:
		return false
	}
}

// cliPreviewFallbackOrder returns preview model candidates for a base model.
func cliPreviewFallbackOrder(model string) []string {
	switch model {
	case "gemini-2.5-pro":
		return []string{
			// "gemini-2.5-pro-preview-05-06",
			// "gemini-2.5-pro-preview-06-05",
		}
	case "gemini-2.5-flash":
		return []string{
			// "gemini-2.5-flash-preview-04-17",
			// "gemini-2.5-flash-preview-05-20",
		}
	case "gemini-2.5-flash-lite":
		return []string{
			// "gemini-2.5-flash-lite-preview-06-17",
		}
	default:
		return nil
	}
}

func fixGeminiCLIImageAspectRatio(modelName string, rawJSON []byte) []byte {
	if modelName == "gemini-2.5-flash-image-preview" {
		aspectRatioResult := gjson.GetBytes(rawJSON, "request.generationConfig.imageConfig.aspectRatio")
		if aspectRatioResult.Exists() {
			contents := gjson.GetBytes(rawJSON, "request.contents")
			contentArray := contents.Array()
			if len(contentArray) > 0 {
				hasInlineData := false
			loopContent:
				for i := 0; i < len(contentArray); i++ {
					parts := contentArray[i].Get("parts").Array()
					for j := 0; j < len(parts); j++ {
						if parts[j].Get("inlineData").Exists() {
							hasInlineData = true
							break loopContent
						}
					}
				}

				if !hasInlineData {
					emptyImageBase64ed, _ := util.CreateWhiteImageBase64(aspectRatioResult.String())
					emptyImagePart := `{"inlineData":{"mime_type":"image/png","data":""}}`
					emptyImagePart, _ = sjson.Set(emptyImagePart, "inlineData.data", emptyImageBase64ed)
					newPartsJSON := `[]`
					newPartsJSON, _ = sjson.SetRaw(newPartsJSON, "-1", `{"text": "Based on the following requirements, create an image within the uploaded picture. The new content *MUST* completely cover the entire area of the original picture, maintaining its exact proportions, and *NO* blank areas should appear."}`)
					newPartsJSON, _ = sjson.SetRaw(newPartsJSON, "-1", emptyImagePart)

					parts := contentArray[0].Get("parts").Array()
					for j := 0; j < len(parts); j++ {
						newPartsJSON, _ = sjson.SetRaw(newPartsJSON, "-1", parts[j].Raw)
					}

					rawJSON, _ = sjson.SetRawBytes(rawJSON, "request.contents.0.parts", []byte(newPartsJSON))
					rawJSON, _ = sjson.SetRawBytes(rawJSON, "request.generationConfig.responseModalities", []byte(`["IMAGE", "TEXT"]`))
				}
			}
			rawJSON, _ = sjson.DeleteBytes(rawJSON, "request.generationConfig.imageConfig")
		}
	}
	return rawJSON
}
