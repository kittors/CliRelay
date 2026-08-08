package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	safetySafe          = "Safe"
	safetyControversial = "Controversial"
	safetyUnsafe        = "Unsafe"

	// guardMaxTokens follows the Qwen3Guard model card's max_new_tokens=128.
	// A tighter cap risks truncating the "Categories:" line mid-list: the
	// prefix has already been emitted at that point, so parsing succeeds and
	// silently loses the tail categories — exactly when the most categories
	// matched, i.e. the worst moment to under-report.
	guardMaxTokens = 128
)

var errGuardInvalidResponse = errors.New("qwen3guard response is not a valid verdict")

// guardVerdict is one parsed Qwen3Guard reply, before profile policy applies.
type guardVerdict struct {
	Safety            string
	Categories        []string
	UnknownCategories []string
}

// qwen3GuardBackend drives a Qwen3Guard-Gen model over an OpenAI-compatible
// chat completions endpoint (vLLM, SGLang, Ollama or a hosted equivalent). The
// model replies with a fixed two-line text verdict rather than scores:
//
//	Safety: Safe | Controversial | Unsafe
//	Categories: Violent, Jailbreak, ...
type qwen3GuardBackend struct {
	httpClient *http.Client
}

func (b *qwen3GuardBackend) Check(ctx context.Context, profile Profile, input string) (backendResult, error) {
	// TimeoutMS budgets the entire evaluation. Chunks run under one deadline so
	// a long prompt cannot multiply the latency the caller was promised.
	budgetCtx, cancel := context.WithTimeout(ctx, time.Duration(profile.TimeoutMS)*time.Millisecond)
	defer cancel()

	chunks, truncatedRunes := splitForGuard(input, profile.InputLimit, profile.MaxChunks)
	if len(chunks) == 0 {
		return backendResult{}, nil
	}
	if truncatedRunes > 0 {
		// Never drop input silently: an unscanned tail is a coverage gap the
		// operator has to be able to see and fix by raising the limits.
		log.WithFields(log.Fields{
			"profile_id":      profile.ID,
			"input_limit":     profile.InputLimit,
			"max_chunks":      profile.MaxChunks,
			"truncated_chars": truncatedRunes,
		}).Warn("content moderation truncated input before qwen3guard scan")
	}

	aggregate := backendResult{Scores: map[string]float64{}}
	categories := make(map[string]struct{})
	matched := make(map[string]struct{})
	unknown := make(map[string]struct{})
	// Chunks run serially: guard deployments are typically a single small
	// instance, and firing every chunk at once would only stall them all.
	for _, chunk := range chunks {
		verdict, err := b.scanChunk(budgetCtx, profile, chunk)
		if err != nil {
			return backendResult{}, err
		}
		chunkMatched, block := decideGuardVerdict(profile, verdict)
		if guardSeverity(verdict.Safety) > guardSeverity(aggregate.Safety) {
			aggregate.Safety = verdict.Safety
		}
		for _, category := range verdict.Categories {
			categories[category] = struct{}{}
		}
		for _, category := range verdict.UnknownCategories {
			unknown[category] = struct{}{}
		}
		score := guardScore(verdict.Safety)
		for _, category := range chunkMatched {
			matched[category] = struct{}{}
			if score > aggregate.Scores[category] {
				aggregate.Scores[category] = score
			}
		}
		if block {
			aggregate.Block = true
			break
		}
	}
	aggregate.Categories = orderedScannerKeys(categories)
	aggregate.MatchedScanners = orderedScannerKeys(matched)
	aggregate.UnknownCategories = sortedKeys(unknown)
	return aggregate, nil
}

func (b *qwen3GuardBackend) scanChunk(ctx context.Context, profile Profile, chunk string) (guardVerdict, error) {
	endpoint, err := moderationEndpointURL(profile.BaseURL, "/v1/chat/completions")
	if err != nil {
		return guardVerdict{}, err
	}
	// No "seed": the model card does not ask for it and temperature 0 is
	// already deterministic enough for classification, so we avoid sending a
	// non-standard field that some OpenAI-compatible servers reject outright.
	payload, err := json.Marshal(map[string]any{
		"model":       profile.Model,
		"messages":    []map[string]string{{"role": "user", "content": chunk}},
		"temperature": 0,
		"max_tokens":  guardMaxTokens,
	})
	if err != nil {
		return guardVerdict{}, fmt.Errorf("encode qwen3guard request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return guardVerdict{}, fmt.Errorf("create qwen3guard request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Self-hosted guard endpoints frequently run unauthenticated.
	if profile.APIKeySecret != "" {
		req.Header.Set("Authorization", "Bearer "+profile.APIKeySecret)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return guardVerdict{}, fmt.Errorf("qwen3guard request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return guardVerdict{}, fmt.Errorf("qwen3guard API returned status %d", resp.StatusCode)
	}
	body, err := readModerationResponse(resp)
	if err != nil {
		return guardVerdict{}, err
	}
	content, err := extractChatContent(body)
	if err != nil {
		return guardVerdict{}, err
	}
	return parseQwen3Guard(content)
}

// parseQwen3Guard reads the model's verdict lines.
//
// Line-prefix matching is deliberately stricter than the regex in the model
// card: a duplicated Safety/Categories line means the model went off-script,
// and picking the first match (as a regex would) would hide that. An invalid
// verdict surfaces as an error so the caller fails open on an explicit
// dependency failure rather than on a guessed verdict.
func parseQwen3Guard(content string) (guardVerdict, error) {
	var safety, categoryLine string
	var sawCategories bool
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "safety:"):
			if safety != "" {
				return guardVerdict{}, errGuardInvalidResponse
			}
			safety = strings.TrimSpace(line[len("safety:"):])
		case strings.HasPrefix(lower, "categories:"):
			if sawCategories {
				return guardVerdict{}, errGuardInvalidResponse
			}
			sawCategories = true
			categoryLine = strings.TrimSpace(line[len("categories:"):])
		default:
			// Response-moderation emits an extra "Refusal:" line and models may
			// append prose. Neither affects a prompt-moderation verdict.
		}
	}
	switch strings.ToLower(safety) {
	case "safe":
		safety = safetySafe
	case "controversial":
		safety = safetyControversial
	case "unsafe":
		safety = safetyUnsafe
	default:
		return guardVerdict{}, errGuardInvalidResponse
	}
	if !sawCategories {
		return guardVerdict{}, errGuardInvalidResponse
	}
	known := make(map[string]struct{})
	unknown := make(map[string]struct{})
	for _, raw := range strings.Split(categoryLine, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.EqualFold(raw, "none") || strings.EqualFold(raw, "n/a") {
			continue
		}
		category := NormalizeCategory(raw)
		if IsScannerID(category) {
			known[category] = struct{}{}
		} else {
			unknown[unknownCategoryID(category)] = struct{}{}
		}
	}
	return guardVerdict{
		Safety:            safety,
		Categories:        orderedScannerKeys(known),
		UnknownCategories: sortedKeys(unknown),
	}, nil
}

// decideGuardVerdict applies profile policy to one parsed verdict and reports
// the enabled categories it matched.
func decideGuardVerdict(profile Profile, verdict guardVerdict) ([]string, bool) {
	enabled := profile.Scanners
	// An empty scanner list means the operator watches every category.
	if len(enabled) == 0 {
		enabled = AllScannerIDs
	}
	enabledSet := make(map[string]struct{}, len(enabled))
	for _, category := range enabled {
		enabledSet[category] = struct{}{}
	}
	matched := make([]string, 0, len(verdict.Categories))
	for _, category := range verdict.Categories {
		if _, ok := enabledSet[category]; ok {
			matched = append(matched, category)
		}
	}
	switch verdict.Safety {
	case safetyUnsafe:
		// The model flagged content, but if every category it named is one the
		// operator switched off, respect that and allow. Unnamed or
		// out-of-catalog risks still block: we cannot prove they were opted out.
		if len(matched) == 0 && len(verdict.Categories) > 0 && len(verdict.UnknownCategories) == 0 {
			return matched, false
		}
		return matched, true
	case safetyControversial:
		switch profile.ControversialAction {
		case ControversialActionBlock:
			return matched, true
		case ControversialActionAllow:
			return matched, false
		default:
			// elevated_only: block only on categories whose false-negative cost
			// outweighs a false positive (see DefaultElevatedCategories).
			elevated := make(map[string]struct{}, len(profile.ElevatedCategories))
			for _, category := range profile.ElevatedCategories {
				elevated[category] = struct{}{}
			}
			for _, category := range matched {
				if _, ok := elevated[category]; ok {
					return matched, true
				}
			}
			return matched, false
		}
	default:
		return matched, false
	}
}

// splitForGuard slices input into rune-safe chunks, returning how many runes
// were dropped past the max_chunks ceiling.
func splitForGuard(value string, limit, maxChunks int) ([]string, int) {
	if limit <= 0 {
		limit = DefaultInputLimit
	}
	if maxChunks <= 0 {
		maxChunks = DefaultMaxChunks
	}
	runes := []rune(value)
	chunks := make([]string, 0, maxChunks)
	for start := 0; start < len(runes); start += limit {
		if len(chunks) == maxChunks {
			return chunks, len(runes) - start
		}
		end := start + limit
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks, 0
}

// extractChatContent pulls the assistant text out of a chat completion, also
// accepting the content-block array form some servers return.
func extractChatContent(body []byte) (string, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) == 0 {
		return "", fmt.Errorf("decode qwen3guard response: %w", errGuardInvalidResponse)
	}
	switch typed := response.Choices[0].Message.Content.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return "", errGuardInvalidResponse
		}
		return typed, nil
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := object["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) == 0 {
			return "", errGuardInvalidResponse
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", errGuardInvalidResponse
	}
}

func guardSeverity(safety string) int {
	switch safety {
	case safetyUnsafe:
		return 3
	case safetyControversial:
		return 2
	case safetySafe:
		return 1
	default:
		return 0
	}
}

// guardScore projects the three-state verdict onto the numeric scale the
// existing Decision fields, logs and admin UI already understand.
func guardScore(safety string) float64 {
	switch safety {
	case safetyUnsafe:
		return 1
	case safetyControversial:
		return 0.5
	default:
		return 0
	}
}
