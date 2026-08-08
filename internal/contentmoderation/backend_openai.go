package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// openaiModerationsBackend targets the OpenAI /v1/moderations API, which
// returns a per-category score map compared against the profile thresholds.
type openaiModerationsBackend struct {
	httpClient *http.Client
}

func (b *openaiModerationsBackend) Check(ctx context.Context, profile Profile, input string) (backendResult, error) {
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
