package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
)

// openAIModerationMaxInputChars caps the text sent in one moderations call.
//
// OpenAI documents no text length limit for omni-moderation-latest: the model
// page, the moderation guide and the API reference (checked 2026-10-29) only
// limit image size. Third-party model catalogs list a 32,768-token context,
// and the model page's default rate limits are 10,000 tokens per minute on the
// free tier and 20,000 on the first paid tier, which a single oversized call
// can exceed by itself. Either failure used to fail open, so an over-long
// request went unmoderated. 8,000 characters stays inside 32,768 tokens even
// at four tokens per character, the worst case for rare CJK characters and
// emoji, and is a small share of a paid tier's minute budget. The older OpenAI
// guide also advised chunks under about 2,000 characters for accuracy, so this
// is not a limit to raise lightly.
const openAIModerationMaxInputChars = 8000

// openaiModerationsBackend targets the OpenAI /v1/moderations API, which
// returns a per-category score map compared against the profile thresholds.
type openaiModerationsBackend struct {
	httpClient *http.Client
}

func (b *openaiModerationsBackend) Check(ctx context.Context, profile Profile, segments []Segment) (backendResult, error) {
	// One string per call: a single input keeps OpenAI-compatible endpoints
	// that do not accept the array form working. Past the cap, the oldest text
	// is skipped and the newest user message always stays in.
	input, skipped := selectNewestText(segments, openAIModerationMaxInputChars)
	if input == "" {
		return backendResult{}, nil
	}
	logSkipped(profile, skipped, utf8.RuneCountInString(input), "content moderation skipped the oldest input before openai moderations scan",
		log.Fields{"max_input_chars": openAIModerationMaxInputChars})
	endpoint, err := moderationEndpointURL(profile.BaseURL, "/v1/moderations")
	if err != nil {
		return backendResult{}, err
	}
	payload, err := json.Marshal(struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}{Model: profile.Model, Input: input})
	if err != nil {
		return backendResult{}, fmt.Errorf("encode moderation request: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(profile.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return backendResult{}, fmt.Errorf("create moderation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+profile.APIKeySecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return backendResult{}, fmt.Errorf("moderation request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return backendResult{}, fmt.Errorf("moderation API returned status %d", resp.StatusCode)
	}
	body, err := readModerationResponse(resp)
	if err != nil {
		return backendResult{}, err
	}
	var result struct {
		Results []struct {
			CategoryScores map[string]float64 `json:"category_scores"`
		} `json:"results"`
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return backendResult{}, fmt.Errorf("decode moderation response: %w", err)
	}
	if len(result.Results) == 0 || result.Results[0].CategoryScores == nil {
		return backendResult{}, fmt.Errorf("moderation response missing category scores")
	}
	scores := result.Results[0].CategoryScores
	thresholds := mergeThresholds(profile.Thresholds)
	block := false
	for category, score := range scores {
		if threshold, ok := thresholds[category]; ok && score >= threshold {
			block = true
		}
	}
	return backendResult{Scores: scores, Block: block}, nil
}
