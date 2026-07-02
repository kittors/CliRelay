package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const (
	defaultCodexFingerprintRecommendationDays  = 7
	defaultCodexFingerprintRecommendationLimit = 200
	maxCodexFingerprintRecommendationDays      = 30
	maxCodexFingerprintRecommendationLimit     = 1000
	maxCodexFingerprintSamples                 = 3
	maxCodexFingerprintDisplayValue            = 240
	codexFingerprintRecommendationQueryTimeout = 8 * time.Second
)

type CodexFingerprintRecommendationQuery struct {
	Days  int
	Limit int
}

type CodexFingerprintRecommendationResult struct {
	Items     []CodexFingerprintRecommendation `json:"items"`
	Days      int                              `json:"days"`
	Limit     int                              `json:"limit"`
	Inspected int                              `json:"inspected"`
	Matched   int                              `json:"matched"`
}

type CodexFingerprintRecommendation struct {
	ID             string                                 `json:"id"`
	Count          int                                    `json:"count"`
	FirstSeenAt    time.Time                              `json:"first_seen_at"`
	LastSeenAt     time.Time                              `json:"last_seen_at"`
	Headers        map[string]string                      `json:"headers"`
	Recommended    config.CodexIdentityFingerprintConfig  `json:"recommended"`
	IgnoredHeaders map[string]string                      `json:"ignored_headers,omitempty"`
	Samples        []CodexFingerprintRecommendationSample `json:"samples"`
}

type CodexFingerprintRecommendationSample struct {
	LogID       int64     `json:"log_id"`
	Timestamp   time.Time `json:"timestamp"`
	Model       string    `json:"model"`
	Source      string    `json:"source"`
	ChannelName string    `json:"channel_name"`
	AuthIndex   string    `json:"auth_index"`
	Failed      bool      `json:"failed"`
	Method      string    `json:"method,omitempty"`
	Path        string    `json:"path,omitempty"`
	Host        string    `json:"host,omitempty"`
	IP          string    `json:"ip,omitempty"`
}

type codexFingerprintDetailRow struct {
	logID       int64
	timestamp   time.Time
	model       string
	source      string
	channelName string
	authIndex   string
	failed      bool
	detail      string
}

type codexFingerprintClientDetail struct {
	ip                 string
	method             string
	path               string
	host               string
	fingerprintHeaders http.Header
	headers            http.Header
}

type codexFingerprintParsedCandidate struct {
	headers        map[string]string
	ignoredHeaders map[string]string
	recommended    config.CodexIdentityFingerprintConfig
}

// QueryCodexFingerprintRecommendations builds candidate Codex identity templates
// from recent request details captured from real terminal traffic.
func QueryCodexFingerprintRecommendations(params CodexFingerprintRecommendationQuery) (CodexFingerprintRecommendationResult, error) {
	params = normalizeCodexFingerprintRecommendationQuery(params)
	result := CodexFingerprintRecommendationResult{
		Items: make([]CodexFingerprintRecommendation, 0),
		Days:  params.Days,
		Limit: params.Limit,
	}

	db := getReadDB()
	if db == nil {
		return result, nil
	}

	rows, err := queryRecentCodexFingerprintDetailRows(db, params)
	if err != nil {
		return CodexFingerprintRecommendationResult{}, err
	}
	result.Inspected = len(rows)

	byKey := make(map[string]*CodexFingerprintRecommendation)
	for _, row := range rows {
		client, ok := parseCodexFingerprintClientDetail(row.detail)
		if !ok {
			continue
		}
		headers := client.fingerprintHeaders
		if len(headers) == 0 {
			headers = client.headers
		}
		parsed, ok := codexFingerprintCandidateFromHeaders(headers)
		if !ok {
			continue
		}
		result.Matched++

		key := codexFingerprintRecommendationKey(parsed.recommended)
		item, exists := byKey[key]
		if !exists {
			item = &CodexFingerprintRecommendation{
				ID:             codexFingerprintRecommendationID(key),
				Headers:        parsed.headers,
				Recommended:    parsed.recommended,
				IgnoredHeaders: parsed.ignoredHeaders,
				FirstSeenAt:    row.timestamp,
				LastSeenAt:     row.timestamp,
				Samples:        make([]CodexFingerprintRecommendationSample, 0, maxCodexFingerprintSamples),
			}
			byKey[key] = item
		}
		item.IgnoredHeaders = mergeCodexFingerprintHeaders(item.IgnoredHeaders, parsed.ignoredHeaders)
		item.Count++
		if row.timestamp.Before(item.FirstSeenAt) {
			item.FirstSeenAt = row.timestamp
		}
		if row.timestamp.After(item.LastSeenAt) {
			item.LastSeenAt = row.timestamp
		}
		if len(item.Samples) < maxCodexFingerprintSamples {
			item.Samples = append(item.Samples, codexFingerprintSampleFromRow(row, client))
		}
	}

	for _, item := range byKey {
		result.Items = append(result.Items, *item)
	}
	sort.Slice(result.Items, func(i, j int) bool {
		left := result.Items[i]
		right := result.Items[j]
		if left.Count != right.Count {
			return left.Count > right.Count
		}
		if !left.LastSeenAt.Equal(right.LastSeenAt) {
			return left.LastSeenAt.After(right.LastSeenAt)
		}
		return left.ID < right.ID
	})

	return result, nil
}

func mergeCodexFingerprintHeaders(target map[string]string, source map[string]string) map[string]string {
	for key, value := range source {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		if target == nil {
			target = make(map[string]string)
		}
		if _, exists := target[key]; !exists {
			target[key] = value
		}
	}
	return target
}

func normalizeCodexFingerprintRecommendationQuery(params CodexFingerprintRecommendationQuery) CodexFingerprintRecommendationQuery {
	if params.Days < 1 {
		params.Days = defaultCodexFingerprintRecommendationDays
	}
	if params.Days > maxCodexFingerprintRecommendationDays {
		params.Days = maxCodexFingerprintRecommendationDays
	}
	if params.Limit < 1 {
		params.Limit = defaultCodexFingerprintRecommendationLimit
	}
	if params.Limit > maxCodexFingerprintRecommendationLimit {
		params.Limit = maxCodexFingerprintRecommendationLimit
	}
	return params
}

func queryRecentCodexFingerprintDetailRows(db *sql.DB, params CodexFingerprintRecommendationQuery) ([]codexFingerprintDetailRow, error) {
	cutoff := CutoffStartUTC(params.Days).Format(time.RFC3339)
	ctx, cancel := context.WithTimeout(context.Background(), codexFingerprintRecommendationQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(
		ctx,
		`WITH recent_content AS (
		     SELECT log_id, timestamp, compression, detail_content
		       FROM request_log_content
		      WHERE timestamp >= ?
		        AND length(detail_content) > 0
		      ORDER BY timestamp DESC
		      LIMIT ?
		   )
		 SELECT logs.id, logs.timestamp, logs.model, logs.source, logs.channel_name, logs.auth_index,
		        logs.failed, recent_content.compression, recent_content.detail_content
		   FROM recent_content
		   JOIN request_logs logs ON logs.id = recent_content.log_id
		  ORDER BY recent_content.timestamp DESC`,
		cutoff,
		params.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: query codex fingerprint details: %w", err)
	}
	defer rows.Close()

	result := make([]codexFingerprintDetailRow, 0, params.Limit)
	for rows.Next() {
		var (
			row         codexFingerprintDetailRow
			timestamp   string
			failedInt   int
			compression string
			compressed  []byte
		)
		if err := rows.Scan(
			&row.logID,
			&timestamp,
			&row.model,
			&row.source,
			&row.channelName,
			&row.authIndex,
			&failedInt,
			&compression,
			&compressed,
		); err != nil {
			return nil, fmt.Errorf("usage: scan codex fingerprint detail row: %w", err)
		}
		if parsed, ok := parseStoredTime(timestamp); ok {
			row.timestamp = parsed
		}
		row.failed = failedInt != 0
		detail, err := decompressLogContent(compression, compressed)
		if err != nil {
			return nil, err
		}
		row.detail = detail
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate codex fingerprint detail rows: %w", err)
	}
	return result, nil
}

func parseCodexFingerprintClientDetail(detail string) (codexFingerprintClientDetail, bool) {
	var payload struct {
		Client map[string]any `json:"client"`
	}
	if strings.TrimSpace(detail) == "" {
		return codexFingerprintClientDetail{}, false
	}
	if err := json.Unmarshal([]byte(detail), &payload); err != nil || payload.Client == nil {
		return codexFingerprintClientDetail{}, false
	}
	client := codexFingerprintClientDetail{
		ip:                 stringValue(payload.Client["ip"]),
		method:             stringValue(payload.Client["method"]),
		path:               stringValue(payload.Client["path"]),
		host:               stringValue(payload.Client["host"]),
		fingerprintHeaders: headerMapFromAny(payload.Client["fingerprint_headers"]),
		headers:            headerMapFromAny(payload.Client["headers"]),
	}
	return client, len(client.fingerprintHeaders) > 0 || len(client.headers) > 0
}

func headerMapFromAny(raw any) http.Header {
	record, ok := raw.(map[string]any)
	if !ok || len(record) == 0 {
		return http.Header{}
	}
	headers := http.Header{}
	for key, value := range record {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		for _, item := range headerValuesFromAny(value) {
			item = strings.TrimSpace(item)
			if item != "" {
				headers.Add(key, item)
			}
		}
	}
	return headers
}

func headerValuesFromAny(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []string:
		return typed
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := stringValue(item); text != "" {
				values = append(values, text)
			}
		}
		return values
	default:
		text := stringValue(value)
		if text == "" {
			return nil
		}
		return []string{text}
	}
}

func codexFingerprintCandidateFromHeaders(headers http.Header) (codexFingerprintParsedCandidate, bool) {
	userAgent := firstHeaderValue(headers, "User-Agent")
	version := firstHeaderValue(headers, "Version")
	originator := firstHeaderValue(headers, "Originator")
	openAIBeta := firstHeaderValue(headers, "OpenAI-Beta")
	codexBetaFeatures := firstHeaderValue(headers, "X-Codex-Beta-Features")

	recommended := config.CodexIdentityFingerprintConfig{
		Enabled:       true,
		SessionMode:   config.DefaultCodexFingerprintSessionMode,
		CustomHeaders: map[string]string{},
	}
	if userAgent != "" {
		recommended.UserAgent = userAgent
	}
	if version != "" {
		recommended.Version = version
	}
	if originator != "" {
		recommended.Originator = originator
	}
	if strings.Contains(strings.ToLower(openAIBeta), "responses_websockets=") {
		recommended.WebsocketBeta = openAIBeta
	}
	if codexBetaFeatures != "" {
		recommended.BetaFeatures = codexBetaFeatures
	}

	if !looksLikeCodexHeaders(headers, userAgent, originator, codexBetaFeatures, openAIBeta) {
		return codexFingerprintParsedCandidate{}, false
	}
	if recommended.UserAgent == "" && recommended.Version == "" && recommended.Originator == "" &&
		recommended.WebsocketBeta == "" && recommended.BetaFeatures == "" && len(recommended.CustomHeaders) == 0 {
		return codexFingerprintParsedCandidate{}, false
	}

	observed := make(map[string]string)
	addObservedHeader(observed, "User-Agent", userAgent)
	addObservedHeader(observed, "Version", version)
	addObservedHeader(observed, "Originator", originator)
	addObservedHeader(observed, "OpenAI-Beta", openAIBeta)
	addObservedHeader(observed, "X-Codex-Beta-Features", codexBetaFeatures)

	ignored := codexFingerprintIgnoredHeaders(headers)
	return codexFingerprintParsedCandidate{
		headers:        observed,
		ignoredHeaders: ignored,
		recommended:    recommended,
	}, true
}

func looksLikeCodexHeaders(headers http.Header, userAgent, originator, codexBetaFeatures, openAIBeta string) bool {
	if containsFold(userAgent, "codex") ||
		containsFold(originator, "codex") ||
		strings.TrimSpace(codexBetaFeatures) != "" ||
		containsFold(openAIBeta, "responses_websockets=") {
		return true
	}
	for key := range headers {
		if containsFold(key, "codex") {
			return true
		}
	}
	return false
}

func codexFingerprintIgnoredHeaders(headers http.Header) map[string]string {
	ignored := make(map[string]string)
	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(key))
		normalized := strings.ToLower(canonical)
		if canonical == "" || len(values) == 0 {
			continue
		}
		switch {
		case normalized == "session_id" || normalized == "session-id" || strings.Contains(normalized, "session"):
			addObservedHeader(ignored, canonical, maskCodexFingerprintHeaderValue(values[0]))
		case normalized == "x-codex-turn-metadata" || normalized == "x-codex-turn-state" ||
			normalized == "x-client-request-id" || normalized == "conversation_id":
			addObservedHeader(ignored, canonical, truncateCodexFingerprintValue(values[0]))
		}
	}
	if len(ignored) == 0 {
		return nil
	}
	return ignored
}

func addObservedHeader(target map[string]string, key, value string) {
	value = truncateCodexFingerprintValue(value)
	if strings.TrimSpace(key) == "" || value == "" {
		return
	}
	target[http.CanonicalHeaderKey(strings.TrimSpace(key))] = value
}

func firstHeaderValue(headers http.Header, key string) string {
	if len(headers) == 0 {
		return ""
	}
	if value := strings.TrimSpace(headers.Get(key)); value != "" {
		return value
	}
	for existing, values := range headers {
		if strings.EqualFold(existing, key) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func codexFingerprintRecommendationKey(recommended config.CodexIdentityFingerprintConfig) string {
	parts := []string{
		"user-agent=" + strings.TrimSpace(recommended.UserAgent),
		"version=" + strings.TrimSpace(recommended.Version),
		"originator=" + strings.TrimSpace(recommended.Originator),
		"websocket-beta=" + strings.TrimSpace(recommended.WebsocketBeta),
	}
	customKeys := make([]string, 0, len(recommended.CustomHeaders))
	for key := range recommended.CustomHeaders {
		customKeys = append(customKeys, key)
	}
	sort.Strings(customKeys)
	for _, key := range customKeys {
		parts = append(parts, "custom:"+strings.TrimSpace(key)+"="+strings.TrimSpace(recommended.CustomHeaders[key]))
	}
	return strings.Join(parts, "\n")
}

func codexFingerprintRecommendationID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

func codexFingerprintSampleFromRow(row codexFingerprintDetailRow, client codexFingerprintClientDetail) CodexFingerprintRecommendationSample {
	return CodexFingerprintRecommendationSample{
		LogID:       row.logID,
		Timestamp:   row.timestamp,
		Model:       row.model,
		Source:      row.source,
		ChannelName: row.channelName,
		AuthIndex:   row.authIndex,
		Failed:      row.failed,
		Method:      client.method,
		Path:        client.path,
		Host:        client.host,
		IP:          client.ip,
	}
}

func truncateCodexFingerprintValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxCodexFingerprintDisplayValue {
		return value
	}
	return value[:maxCodexFingerprintDisplayValue-3] + "..."
}

func maskCodexFingerprintHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 12 {
		return "****"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func containsFold(value, needle string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(needle))
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}
