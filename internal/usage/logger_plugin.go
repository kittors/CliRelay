// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
// It includes plugins for monitoring API usage, token consumption, and other metrics
// to help with observability and billing purposes.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

var (
	statisticsEnabled atomic.Bool
	redisClient       *redis.Client
	// redisCtx 代表 usage Redis 同步器的服务级后台上下文：
	// - owner: InitRedis / StopRedis
	// - 取消条件: StopRedis
	// - 超时策略: 周期 ticker 驱动；单次 Redis 命令使用客户端默认/调用方超时
	// - 清理方式: redisCancel + redisSyncWg 等待后台同步循环退出
	redisCtx    = context.Background()
	redisCancel context.CancelFunc
	redisSyncWg sync.WaitGroup
)

const redisUsageKey = "cliproxy:usage_stats_snapshot"

func init() {
	statisticsEnabled.Store(true)
	coreusage.RegisterPlugin(NewLoggerPlugin())
}

// LoggerPlugin collects in-memory request statistics for usage analysis.
// It implements coreusage.Plugin to receive usage records emitted by the runtime.
type LoggerPlugin struct {
	stats *RequestStatistics
}

// NewLoggerPlugin constructs a new logger plugin instance.
//
// Returns:
//   - *LoggerPlugin: A new logger plugin instance wired to the shared statistics store.
func NewLoggerPlugin() *LoggerPlugin { return &LoggerPlugin{stats: defaultRequestStatistics} }

// HandleUsage implements coreusage.Plugin.
// It updates the in-memory statistics store whenever a usage record is received.
//
// Parameters:
//   - ctx: The context for the usage record
//   - record: The usage record to aggregate
func (p *LoggerPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if !statisticsEnabled.Load() {
		return
	}
	if p == nil || p.stats == nil {
		return
	}
	p.stats.Record(ctx, record)
}

// SetStatisticsEnabled toggles whether in-memory statistics are recorded.
func SetStatisticsEnabled(enabled bool) { statisticsEnabled.Store(enabled) }

// StatisticsEnabled reports the current recording state.
func StatisticsEnabled() bool { return statisticsEnabled.Load() }

// InitRedis initializes the Redis client for usage persistence and starts the sync loop.
func InitRedis(cfg config.RedisConfig) {
	if !cfg.Enable || cfg.Addr == "" {
		return
	}

	redisClient = redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	if err := redisClient.Ping(redisCtx).Err(); err != nil {
		log.Errorf("failed to connect to Redis for usage persistence: %v", err)
		redisClient.Close()
		redisClient = nil
		return
	}

	log.Infof("Redis persistence for usage statistics activated on %s (DB: %d)", cfg.Addr, cfg.DB)

	// Attempt to load existing data
	if err := loadFromRedis(); err != nil {
		log.Errorf("failed to load usage statistics from Redis: %v", err)
	} else {
		log.Infof("Successfully loaded usage statistics from Redis")
	}

	// Migrate existing in-memory data to SQLite (first run only)
	if defaultRequestStatistics != nil {
		snapshot := defaultRequestStatistics.Snapshot()
		if migrated, err := MigrateFromSnapshot(snapshot); err != nil {
			log.Errorf("usage: migration from Redis snapshot failed: %v", err)
		} else if migrated > 0 {
			log.Infof("usage: migrated %d records from Redis to SQLite", migrated)
		}
	}

	// Redis 同步循环独立于单次请求，受服务 shutdown 控制。
	redisCtx, redisCancel = context.WithCancel(context.Background())
	redisSyncWg.Add(1)
	go redisSyncLoop()
}

// StopRedis flushes the latest snapshot to Redis and closes the client.
func StopRedis() {
	if redisCancel != nil {
		redisCancel()
		redisSyncWg.Wait()
	}
	if redisClient != nil {
		saveToRedis() // Perform a final save
		redisClient.Close()
		log.Infof("Redis usage persistence flushed and closed")
	}
	CloseDB()
}

func redisSyncLoop() {
	defer redisSyncWg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			saveToRedis()
		case <-redisCtx.Done():
			return
		}
	}
}

func saveToRedis() {
	if redisClient == nil || defaultRequestStatistics == nil {
		return
	}
	snapshot := defaultRequestStatistics.Snapshot()
	data, err := json.Marshal(snapshot)
	if err != nil {
		log.Errorf("failed to marshal usage snapshot for Redis: %v", err)
		return
	}
	// We don't set an expiration; it should persist indefinitely
	// saveToRedis 可能发生在定时后台循环或 StopRedis 最终 flush 阶段，
	// 不依赖任意请求 context，因此使用根 context。
	if err := redisClient.Set(context.Background(), redisUsageKey, data, 0).Err(); err != nil {
		log.Errorf("failed to save usage snapshot to Redis: %v", err)
	}
}

func loadFromRedis() error {
	if redisClient == nil || defaultRequestStatistics == nil {
		return nil
	}
	// loadFromRedis 属于服务启动期恢复逻辑，不绑定请求生命周期。
	data, err := redisClient.Get(context.Background(), redisUsageKey).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil // Key does not exist, which is fine for the first run
		}
		return err
	}
	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	result := defaultRequestStatistics.MergeSnapshot(snapshot)
	log.Infof("Loaded %d usage records from Redis (skipped %d duplicates)", result.Added, result.Skipped)
	return nil
}

// RequestStatistics maintains aggregated request metrics in memory.
type RequestStatistics struct {
	mu sync.RWMutex

	totalRequests int64
	successCount  int64
	failureCount  int64
	totalTokens   int64

	apis map[string]*apiStats

	requestsByDay  map[string]int64
	requestsByHour map[int]int64
	tokensByDay    map[string]int64
	tokensByHour   map[int]int64
}

// apiStats holds aggregated metrics for a single API key.
type apiStats struct {
	TotalRequests int64
	TotalTokens   int64
	Models        map[string]*modelStats
}

// modelStats holds aggregated metrics for a specific model within an API.
type modelStats struct {
	TotalRequests int64
	TotalTokens   int64
}

// RequestDetail stores the timestamp and token usage for a single request.
type RequestDetail struct {
	Timestamp    time.Time  `json:"timestamp"`
	Source       string     `json:"source"`
	AuthIndex    string     `json:"auth_index"`
	ChannelName  string     `json:"channel_name,omitempty"`
	Tokens       TokenStats `json:"tokens"`
	LatencyMs    int64      `json:"latency_ms,omitempty"`
	FirstTokenMs int64      `json:"first_token_ms,omitempty"`
	Failed       bool       `json:"failed"`
}

// TokenStats captures the token usage breakdown for a request.
type TokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

// StatisticsSnapshot represents an immutable view of the aggregated metrics.
type StatisticsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	SuccessCount  int64 `json:"success_count"`
	FailureCount  int64 `json:"failure_count"`
	TotalTokens   int64 `json:"total_tokens"`

	APIs map[string]APISnapshot `json:"apis"`

	RequestsByDay  map[string]int64 `json:"requests_by_day"`
	RequestsByHour map[string]int64 `json:"requests_by_hour"`
	TokensByDay    map[string]int64 `json:"tokens_by_day"`
	TokensByHour   map[string]int64 `json:"tokens_by_hour"`
}

// APISnapshot summarises metrics for a single API key.
type APISnapshot struct {
	TotalRequests int64                    `json:"total_requests"`
	TotalTokens   int64                    `json:"total_tokens"`
	Models        map[string]ModelSnapshot `json:"models"`
}

// ModelSnapshot summarises metrics for a specific model.
type ModelSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	TotalTokens   int64 `json:"total_tokens"`
}

// SanitizeForPublic strips sensitive fields from all request details in the snapshot.
// This MUST be called before returning data through any public (unauthenticated) endpoint.
func (s *StatisticsSnapshot) SanitizeForPublic() {
	// No-op: Details are no longer stored in the snapshot.
}

var defaultRequestStatistics = NewRequestStatistics()

// GetRequestStatistics returns the shared statistics store.
func GetRequestStatistics() *RequestStatistics { return defaultRequestStatistics }

// NewRequestStatistics constructs an empty statistics store.
func NewRequestStatistics() *RequestStatistics {
	return &RequestStatistics{
		apis:           make(map[string]*apiStats),
		requestsByDay:  make(map[string]int64),
		requestsByHour: make(map[int]int64),
		tokensByDay:    make(map[string]int64),
		tokensByHour:   make(map[int]int64),
	}
}

// Record ingests a new usage record and updates the aggregates.
func (s *RequestStatistics) Record(ctx context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	if !statisticsEnabled.Load() {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	detail := normaliseDetail(record.Detail)
	totalTokens := detail.TotalTokens
	statsKey := record.APIKey
	if statsKey == "" {
		statsKey = resolveAPIIdentifier(ctx, record)
	}
	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	success := !failed
	modelName := record.Model
	if modelName == "" {
		modelName = "unknown"
	}
	dayKey := timestamp.Format("2006-01-02")
	hourKey := timestamp.Hour()

	s.mu.Lock()
	s.totalRequests++
	if success {
		s.successCount++
	} else {
		s.failureCount++
	}
	s.totalTokens += totalTokens

	stats, ok := s.apis[statsKey]
	if !ok {
		stats = &apiStats{Models: make(map[string]*modelStats)}
		s.apis[statsKey] = stats
	}
	s.updateAPIStats(stats, modelName, RequestDetail{
		Timestamp:    timestamp,
		Source:       record.Source,
		AuthIndex:    record.AuthIndex,
		ChannelName:  record.ChannelName,
		Tokens:       detail,
		LatencyMs:    record.LatencyMs,
		FirstTokenMs: record.FirstTokenMs,
		Failed:       failed,
	})

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
	s.mu.Unlock()

	// Persist request logs in the usage manager worker so SQLite writes stay
	// serialized and do not spawn one goroutine per request.
	// Look up the display name for this API key so it's persisted in the log.
	apiKeyName := ""
	if statsKey != "" {
		if row := GetAPIKey(statsKey); row != nil && row.Name != "" {
			apiKeyName = row.Name
		}
	}
	InsertLogWithDetails(statsKey, apiKeyName, modelName, record.Source, record.ChannelName,
		record.AuthIndex, failed, timestamp, record.LatencyMs, record.FirstTokenMs, detail,
		record.InputContent, record.OutputContent, record.DetailContent)
}

func (s *RequestStatistics) updateAPIStats(stats *apiStats, model string, detail RequestDetail) {
	stats.TotalRequests++
	stats.TotalTokens += detail.Tokens.TotalTokens
	modelStatsValue, ok := stats.Models[model]
	if !ok {
		modelStatsValue = &modelStats{}
		stats.Models[model] = modelStatsValue
	}
	modelStatsValue.TotalRequests++
	modelStatsValue.TotalTokens += detail.Tokens.TotalTokens
}

// Snapshot returns a copy of the aggregated metrics for external consumption.
func (s *RequestStatistics) Snapshot() StatisticsSnapshot {
	result := StatisticsSnapshot{}
	if s == nil {
		return result
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result.TotalRequests = s.totalRequests
	result.SuccessCount = s.successCount
	result.FailureCount = s.failureCount
	result.TotalTokens = s.totalTokens

	result.APIs = make(map[string]APISnapshot, len(s.apis))
	for apiName, stats := range s.apis {
		apiSnapshot := APISnapshot{
			TotalRequests: stats.TotalRequests,
			TotalTokens:   stats.TotalTokens,
			Models:        make(map[string]ModelSnapshot, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			apiSnapshot.Models[modelName] = ModelSnapshot{
				TotalRequests: modelStatsValue.TotalRequests,
				TotalTokens:   modelStatsValue.TotalTokens,
			}
		}
		result.APIs[apiName] = apiSnapshot
	}

	result.RequestsByDay = make(map[string]int64, len(s.requestsByDay))
	for k, v := range s.requestsByDay {
		result.RequestsByDay[k] = v
	}

	result.RequestsByHour = make(map[string]int64, len(s.requestsByHour))
	for hour, v := range s.requestsByHour {
		key := formatHour(hour)
		result.RequestsByHour[key] = v
	}

	result.TokensByDay = make(map[string]int64, len(s.tokensByDay))
	for k, v := range s.tokensByDay {
		result.TokensByDay[k] = v
	}

	result.TokensByHour = make(map[string]int64, len(s.tokensByHour))
	for hour, v := range s.tokensByHour {
		key := formatHour(hour)
		result.TokensByHour[key] = v
	}

	return result
}

type MergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

// MergeSnapshot merges an exported statistics snapshot into the current store.
// Existing data is preserved and duplicate request details are skipped.
func (s *RequestStatistics) MergeSnapshot(snapshot StatisticsSnapshot) MergeResult {
	result := MergeResult{}
	if s == nil {
		return result
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// No details loop required anymore

	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		stats, ok := s.apis[apiName]
		if !ok || stats == nil {
			stats = &apiStats{Models: make(map[string]*modelStats)}
			s.apis[apiName] = stats
		} else if stats.Models == nil {
			stats.Models = make(map[string]*modelStats)
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}

			// Migrate counters directly bypassing dedup
			modelTotalTokens := modelSnapshot.TotalTokens
			if modelTotalTokens < 0 {
				modelTotalTokens = 0
			}

			s.totalRequests += modelSnapshot.TotalRequests
			// successCount and failureCount are not accurately split per model in old versions,
			// just lump into successCount for simple migration of aggregates
			s.successCount += modelSnapshot.TotalRequests
			s.totalTokens += modelTotalTokens

			stats.TotalRequests += modelSnapshot.TotalRequests
			stats.TotalTokens += modelTotalTokens

			modelStatsValue, ok := stats.Models[modelName]
			if !ok {
				modelStatsValue = &modelStats{}
				stats.Models[modelName] = modelStatsValue
			}
			modelStatsValue.TotalRequests += modelSnapshot.TotalRequests
			modelStatsValue.TotalTokens += modelTotalTokens

			result.Added += modelSnapshot.TotalRequests
		}
	}

	return result
}

func (s *RequestStatistics) recordImported(apiName, modelName string, stats *apiStats, detail RequestDetail) {
	totalTokens := detail.Tokens.TotalTokens
	if totalTokens < 0 {
		totalTokens = 0
	}

	s.totalRequests++
	if detail.Failed {
		s.failureCount++
	} else {
		s.successCount++
	}
	s.totalTokens += totalTokens

	s.updateAPIStats(stats, modelName, detail)

	dayKey := detail.Timestamp.Format("2006-01-02")
	hourKey := detail.Timestamp.Hour()

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
}

func dedupKey(apiName, modelName string, detail RequestDetail) string {
	timestamp := detail.Timestamp.UTC().Format(time.RFC3339Nano)
	tokens := normaliseTokenStats(detail.Tokens)
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
		apiName,
		modelName,
		timestamp,
		detail.Source,
		detail.AuthIndex,
		detail.Failed,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.TotalTokens,
	)
}

func resolveAPIIdentifier(ctx context.Context, record coreusage.Record) string {
	if ctx != nil {
		if ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil {
			path := ginCtx.FullPath()
			if path == "" && ginCtx.Request != nil {
				path = ginCtx.Request.URL.Path
			}
			method := ""
			if ginCtx.Request != nil {
				method = ginCtx.Request.Method
			}
			if path != "" {
				if method != "" {
					return method + " " + path
				}
				return path
			}
		}
	}
	if record.Provider != "" {
		return record.Provider
	}
	return "unknown"
}

func resolveSuccess(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil {
		return true
	}
	status := ginCtx.Writer.Status()
	if status == 0 {
		return true
	}
	return status < httpStatusBadRequest
}

const httpStatusBadRequest = 400

func normaliseDetail(detail coreusage.Detail) TokenStats {
	tokens := TokenStats{
		InputTokens:     detail.InputTokens,
		OutputTokens:    detail.OutputTokens,
		ReasoningTokens: detail.ReasoningTokens,
		CachedTokens:    detail.CachedTokens,
		TotalTokens:     detail.TotalTokens,
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens + detail.CachedTokens
	}
	return tokens
}

func normaliseTokenStats(tokens TokenStats) TokenStats {
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
}

func formatHour(hour int) string {
	if hour < 0 {
		hour = 0
	}
	hour = hour % 24
	return fmt.Sprintf("%02d", hour)
}
