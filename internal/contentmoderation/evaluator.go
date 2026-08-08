package contentmoderation

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	ActionAllow        = "allow"
	ActionKeywordBlock = "keyword_block"
	ActionAPIBlock     = "api_block"
	ActionGuardBlock   = "guard_block"
	ActionAPIError     = "api_error"
)

type Decision struct {
	WouldBlock      bool               `json:"would_block"`
	Action          string             `json:"action"`
	MatchedKeyword  string             `json:"matched_keyword,omitempty"`
	HighestCategory string             `json:"highest_category,omitempty"`
	HighestScore    float64            `json:"highest_score,omitempty"`
	CategoryScores  map[string]float64 `json:"category_scores"`
	Thresholds      map[string]float64 `json:"thresholds"`
	// Safety, Categories and MatchedScanners come from guard backends that
	// classify into labels instead of per-category scores.
	Safety          string   `json:"safety,omitempty"`
	Categories      []string `json:"categories,omitempty"`
	MatchedScanners []string `json:"matched_scanners,omitempty"`
	LatencyMS       int64    `json:"latency_ms"`
	ModerationError string   `json:"moderation_error,omitempty"`
}

// backendResult is one moderation upstream's verdict, normalized across the
// score-based and label-based backends.
type backendResult struct {
	Scores            map[string]float64
	Safety            string
	Categories        []string
	MatchedScanners   []string
	UnknownCategories []string
	Block             bool
}

// moderationBackend is the pluggable upstream behind a profile. Implementations
// own their wire protocol and their own block decision; the evaluator owns the
// surrounding policy (mode, keywords, fail-open) that is identical for all.
type moderationBackend interface {
	Check(ctx context.Context, profile Profile, input string) (backendResult, error)
}

type Evaluator struct {
	httpClient *http.Client
}

func NewEvaluator(client *http.Client) *Evaluator {
	if client == nil {
		client = http.DefaultClient
	}
	return &Evaluator{httpClient: client}
}

func (e *Evaluator) backendFor(profile Profile) moderationBackend {
	if profile.Backend == BackendQwen3Guard {
		return &qwen3GuardBackend{httpClient: e.httpClient}
	}
	return &openaiModerationsBackend{httpClient: e.httpClient}
}

func (e *Evaluator) Evaluate(ctx context.Context, profile Profile, input string) Decision {
	decision := Decision{Action: ActionAllow, CategoryScores: map[string]float64{}, Thresholds: mergeThresholds(profile.Thresholds)}
	input = strings.TrimSpace(input)
	if profile.Mode != ModePreBlock || input == "" {
		return decision
	}
	if profile.KeywordMode != KeywordModeAPIOnly {
		if keyword, hit := matchKeyword(input, profile.BlockedKeywords); hit {
			decision.WouldBlock = true
			decision.Action = ActionKeywordBlock
			decision.MatchedKeyword = keyword
			return decision
		}
	}
	if profile.KeywordMode == KeywordModeKeywordOnly {
		return decision
	}
	start := time.Now()
	result, err := e.backendFor(profile).Check(ctx, profile, input)
	decision.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		// Fail open: a broken moderation dependency must not take traffic down.
		decision.Action = ActionAPIError
		decision.ModerationError = err.Error()
		return decision
	}
	if result.Scores != nil {
		decision.CategoryScores = result.Scores
	}
	decision.Safety = result.Safety
	decision.Categories = result.Categories
	decision.MatchedScanners = result.MatchedScanners
	for category, score := range decision.CategoryScores {
		if decision.HighestCategory == "" || score > decision.HighestScore {
			decision.HighestCategory = category
			decision.HighestScore = score
		}
	}
	if result.Block {
		decision.WouldBlock = true
		decision.Action = ActionAPIBlock
		if profile.Backend == BackendQwen3Guard {
			decision.Action = ActionGuardBlock
		}
	}
	return decision
}

func matchKeyword(input string, keywords []string) (string, bool) {
	input = strings.ToLower(input)
	for _, keyword := range keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword != "" && strings.Contains(input, strings.ToLower(keyword)) {
			return keyword, true
		}
	}
	return "", false
}
