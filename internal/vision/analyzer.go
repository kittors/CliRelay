package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// AnalyzeRequest carries all context needed for image analysis.
type AnalyzeRequest struct {
	Model      string       // target model for analysis (empty = analyzer default)
	SessionKey SessionKey   // session key for registry tracking
	Query      string       // user's current question (empty on first analysis)
	Existing   ImageSummary // previously accumulated summary (empty on first analysis)
	ImageData  string       // base64 data from current request
	ImageURL   string       // remote URL if not inline
	MIMEType   string       // "image/png", "image/jpeg", etc.
	SourceKind ImageSourceKind
	TurnIndex  int          // conversation turn index
	IsFollowUp bool         // true if this is a follow-up analysis
}

// AnalyzeResponse contains the analysis result.
type AnalyzeResponse struct {
	Summary ImageSummary
}

// ImageAnalyzer is the interface for analyzing image content.
type ImageAnalyzer interface {
	Analyze(ctx context.Context, req AnalyzeRequest) (AnalyzeResponse, error)
	Name() string
}

// OpenCodeGoAnalyzer implements ImageAnalyzer via OpenCodeGo's API.
type OpenCodeGoAnalyzer struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewOpenCodeGoAnalyzer creates a new analyzer pointed at OpenCodeGo's API.
func NewOpenCodeGoAnalyzer(baseURL, apiKey, model string) *OpenCodeGoAnalyzer {
	return &OpenCodeGoAnalyzer{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:       10,
				IdleConnTimeout:    90 * time.Second,
				DisableCompression: false,
			},
		},
	}
}

func (a *OpenCodeGoAnalyzer) Name() string { return "opencode-go" }

func (a *OpenCodeGoAnalyzer) Analyze(ctx context.Context, req AnalyzeRequest) (AnalyzeResponse, error) {
	var prompt string
	if req.IsFollowUp {
		prompt = a.buildFollowUpPrompt(req)
	} else {
		prompt = a.buildInitialPrompt()
	}

	model := a.model
	if req.Model != "" {
		model = req.Model
	}
	body := a.buildRequestBody(model, prompt, req.ImageData, req.MIMEType)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return AnalyzeResponse{}, fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return AnalyzeResponse{}, fmt.Errorf("analyze request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return AnalyzeResponse{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AnalyzeResponse{}, fmt.Errorf("API status %d: %s", resp.StatusCode, string(respBody))
	}

	return a.parseResponse(respBody, req)
}

func (a *OpenCodeGoAnalyzer) buildInitialPrompt() string {
	return `You are an image analyzer for a coding assistant. Describe this image in detail, focusing on:

SUMMARY: 1-2 sentence overall description of what this image shows.
OCR: Any text visible in the image (error messages, code, UI labels).
LAYOUT: The layout structure, key visual elements, their relative positions.
DETAILS: Any other notable details (colors, icons, UI state, highlighted elements).

Be thorough — the model receiving this data cannot see the original image.`
}

func (a *OpenCodeGoAnalyzer) buildFollowUpPrompt(req AnalyzeRequest) string {
	return fmt.Sprintf(`You are an image analyzer for a coding assistant. You previously analyzed this image and provided this summary:

SUMMARY: %s
OCR: %s
LAYOUT: %s
DETAILS: %s

The user is now asking: "%s"

Look at the original image again. Provide ONLY new or supplementary information that was NOT covered in the existing summary above. Focus on answering the user's specific question. If the question is already answered by the existing summary, return empty supplementary details.`,
		req.Existing.Summary,
		strings.Join(req.Existing.OCRHints, "; "),
		strings.Join(req.Existing.LayoutHints, "; "),
		strings.Join(req.Existing.DetailHints, "; "),
		req.Query,
	)
}

func (a *OpenCodeGoAnalyzer) buildRequestBody(model, prompt, imageData, mimeType string) []byte {
	var mime string
	switch {
	case strings.Contains(mimeType, "png"):
		mime = "image/png"
	case strings.Contains(mimeType, "jpeg") || strings.Contains(mimeType, "jpg"):
		mime = "image/jpeg"
	case strings.Contains(mimeType, "gif"):
		mime = "image/gif"
	case strings.Contains(mimeType, "webp"):
		mime = "image/webp"
	default:
		mime = "image/png"
	}

	dataURL := "data:" + mime + ";base64," + imageData

	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": "You are an expert image analyst. Provide structured, detailed descriptions.",
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
					{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
				},
			},
		},
		"max_tokens": 1024,
	}

	out, _ := json.Marshal(body)
	return out
}

func (a *OpenCodeGoAnalyzer) parseResponse(respBody []byte, req AnalyzeRequest) (AnalyzeResponse, error) {
	// Parse the response JSON
	var raw struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return AnalyzeResponse{}, fmt.Errorf("parse response: %w", err)
	}

	if len(raw.Choices) == 0 {
		return AnalyzeResponse{}, fmt.Errorf("empty choices in response")
	}

	content := raw.Choices[0].Message.Content

	// Parse structured content
	summary := parseStructuredResponse(content, req)

	return AnalyzeResponse{Summary: summary}, nil
}

func parseStructuredResponse(content string, req AnalyzeRequest) ImageSummary {
	s := ImageSummary{
		Confidence: "high",
	}

	lines := strings.Split(content, "\n")
	var currentSection string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "SUMMARY"):
			currentSection = "summary"
			s.Summary = extractValue(line)
			log.Debugf("vision: parser summary=%q", s.Summary)
		case strings.HasPrefix(upper, "OCR"):
			currentSection = "ocr"
			if v := extractValue(line); v != "" && len(s.OCRHints) < 5 {
				s.OCRHints = append(s.OCRHints, v)
			}
		case strings.HasPrefix(upper, "LAYOUT"):
			currentSection = "layout"
			if v := extractValue(line); v != "" && len(s.LayoutHints) < 5 {
				s.LayoutHints = append(s.LayoutHints, v)
			}
		case strings.HasPrefix(upper, "DETAILS") || strings.HasPrefix(upper, "DETAIL"):
			currentSection = "details"
			if v := extractValue(line); v != "" && len(s.DetailHints) < 8 {
				s.DetailHints = append(s.DetailHints, v)
			}
		default:
			// Continuation of previous section
			switch currentSection {
			case "ocr":
				if len(s.OCRHints) > 0 && len(s.OCRHints) < 5 && len(line) < 200 {
					s.OCRHints[len(s.OCRHints)-1] += " " + line
				}
			case "layout":
				if len(s.LayoutHints) > 0 && len(s.LayoutHints) < 5 && len(line) < 200 {
					s.LayoutHints[len(s.LayoutHints)-1] += " " + line
				}
			case "details":
				if len(s.DetailHints) > 0 && len(s.DetailHints) < 8 && len(line) < 200 {
					s.DetailHints[len(s.DetailHints)-1] += " " + line
				}
			}
		}
	}

	// Merge sections into summary for the final FullText equivalent
	if req.IsFollowUp && req.Existing.Summary != "" {
		s.Summary = req.Existing.Summary + " | " + s.Summary
	}

	return s
}

func extractValue(line string) string {
	idx := strings.IndexAny(line, ":：")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(line[idx+1:])
}
