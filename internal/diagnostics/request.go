package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const GinKey = "cliproxy.request_diagnostic"

type contextKey struct{}

type RequestDiagnostic struct {
	mu sync.Mutex

	RequestID      string
	Method         string
	OriginalURL    string
	EffectiveURL   string
	OriginalPath   string
	EffectivePath  string
	Host           string
	RemoteAddr     string
	ClientIP       string
	ClientIPSource string
	UserAgent      string
	ContentType    string
	ContentLength  int64

	Route    *RouteSnapshot
	Auth     *AuthSnapshot
	Quota    *QuotaSnapshot
	Upstream *UpstreamSnapshot
	Egress   map[string]any
	Response *ResponseSnapshot
	Body     *BodySnapshot
}

type Snapshot struct {
	RequestID      string            `json:"request_id,omitempty"`
	Method         string            `json:"method,omitempty"`
	OriginalURL    string            `json:"original_url,omitempty"`
	EffectiveURL   string            `json:"effective_url,omitempty"`
	OriginalPath   string            `json:"original_path,omitempty"`
	EffectivePath  string            `json:"effective_path,omitempty"`
	Host           string            `json:"host,omitempty"`
	RemoteAddr     string            `json:"remote_addr,omitempty"`
	ClientIP       string            `json:"client_ip,omitempty"`
	ClientIPSource string            `json:"client_ip_source,omitempty"`
	UserAgent      string            `json:"user_agent,omitempty"`
	ContentType    string            `json:"content_type,omitempty"`
	ContentLength  int64             `json:"content_length,omitempty"`
	Route          *RouteSnapshot    `json:"route,omitempty"`
	Auth           *AuthSnapshot     `json:"auth,omitempty"`
	Quota          *QuotaSnapshot    `json:"quota,omitempty"`
	Upstream       *UpstreamSnapshot `json:"upstream,omitempty"`
	Egress         map[string]any    `json:"egress,omitempty"`
	Response       *ResponseSnapshot `json:"response,omitempty"`
	Body           *BodySnapshot     `json:"body,omitempty"`
}

type RouteSnapshot struct {
	RoutePath string            `json:"route_path,omitempty"`
	Group     string            `json:"group,omitempty"`
	Fallback  string            `json:"fallback,omitempty"`
	CcSwitch  *CcSwitchSnapshot `json:"ccswitch,omitempty"`
}

type CcSwitchSnapshot struct {
	ConfigID             string   `json:"config_id,omitempty"`
	ClientType           string   `json:"client_type,omitempty"`
	DefaultModel         string   `json:"default_model,omitempty"`
	RoutePath            string   `json:"route_path,omitempty"`
	EndpointPath         string   `json:"endpoint_path,omitempty"`
	AllowedChannelGroups []string `json:"allowed_channel_groups,omitempty"`
}

type AuthSnapshot struct {
	Provider   string `json:"provider,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	APIKeyID   string `json:"api_key_id,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
}

type QuotaSnapshot struct {
	DailyLimit         int     `json:"daily_limit,omitempty"`
	TotalQuota         int     `json:"total_quota,omitempty"`
	ConcurrencyLimit   int     `json:"concurrency_limit,omitempty"`
	RPMLimit           int     `json:"rpm_limit,omitempty"`
	TPMLimit           int     `json:"tpm_limit,omitempty"`
	SpendingLimit      float64 `json:"spending_limit,omitempty"`
	DailySpendingLimit float64 `json:"daily_spending_limit,omitempty"`
	Rejected           bool    `json:"rejected,omitempty"`
	RejectedBy         string  `json:"rejected_by,omitempty"`
	Limit              float64 `json:"limit,omitempty"`
	Current            float64 `json:"current,omitempty"`
	ErrorCode          string  `json:"error_code,omitempty"`
	ErrorType          string  `json:"error_type,omitempty"`
	ErrorMessage       string  `json:"error_message,omitempty"`
}

type UpstreamSnapshot struct {
	Attempt      int    `json:"attempt,omitempty"`
	Provider     string `json:"provider,omitempty"`
	AuthID       string `json:"auth_id,omitempty"`
	AuthLabel    string `json:"auth_label,omitempty"`
	URL          string `json:"url,omitempty"`
	Method       string `json:"method,omitempty"`
	Status       int    `json:"status,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type ResponseSnapshot struct {
	Status       int    `json:"status,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	Source       string `json:"source,omitempty"`
}

type BodySnapshot struct {
	ContentType   string `json:"content_type,omitempty"`
	ContentLength int64  `json:"content_length,omitempty"`
	CapturedBytes int    `json:"captured_bytes,omitempty"`
	Redacted      bool   `json:"redacted,omitempty"`
	RedactedBy    string `json:"redacted_by,omitempty"`
	Model         string `json:"model,omitempty"`
	Stream        *bool  `json:"stream,omitempty"`
	InputItems    int    `json:"input_items,omitempty"`
	Messages      int    `json:"messages,omitempty"`
}

func EnsureGin(c *gin.Context, requestID string) *RequestDiagnostic {
	if c == nil {
		return nil
	}
	if existing := FromGin(c); existing != nil {
		if requestID != "" {
			existing.SetRequestID(requestID)
		}
		existing.UpdateRequest(c.Request)
		return existing
	}
	d := &RequestDiagnostic{}
	d.SetRequestID(requestID)
	d.UpdateRequest(c.Request)
	c.Set(GinKey, d)
	if c.Request != nil {
		c.Request = c.Request.WithContext(WithContext(c.Request.Context(), d))
	}
	return d
}

func FromGin(c *gin.Context) *RequestDiagnostic {
	if c == nil {
		return nil
	}
	if raw, exists := c.Get(GinKey); exists {
		if d, ok := raw.(*RequestDiagnostic); ok {
			return d
		}
	}
	if c.Request != nil {
		return FromContext(c.Request.Context())
	}
	return nil
}

func WithContext(ctx context.Context, d *RequestDiagnostic) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, d)
}

func FromContext(ctx context.Context) *RequestDiagnostic {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(contextKey{}).(*RequestDiagnostic)
	return d
}

func (d *RequestDiagnostic) SetRequestID(requestID string) {
	if d == nil || strings.TrimSpace(requestID) == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.RequestID = strings.TrimSpace(requestID)
}

func (d *RequestDiagnostic) UpdateRequest(req *http.Request) {
	if d == nil || req == nil || req.URL == nil {
		return
	}
	url := requestURL(req)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.OriginalURL == "" {
		d.OriginalURL = url
		d.OriginalPath = req.URL.Path
	}
	d.EffectiveURL = url
	d.EffectivePath = req.URL.Path
	d.Method = req.Method
	d.Host = req.Host
	d.RemoteAddr = req.RemoteAddr
	d.UserAgent = req.UserAgent()
	d.ContentType = req.Header.Get("Content-Type")
	d.ContentLength = req.ContentLength
	if ip, source := util.ForwardedClientIP(req); ip != "" {
		d.ClientIP = ip
		d.ClientIPSource = source
	} else if ip := util.RemoteAddrIP(req.RemoteAddr); ip != "" {
		d.ClientIP = ip
		d.ClientIPSource = "remote_addr"
	}
}

func SetOriginalURL(c *gin.Context, rawURL string) {
	d := FromGin(c)
	if d == nil {
		return
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return
	}
	path := rawURL
	if idx := strings.IndexAny(path, "?#"); idx >= 0 {
		path = path[:idx]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.OriginalURL = rawURL
	d.OriginalPath = path
}

func SetEffectiveURL(c *gin.Context, rawURL string) {
	d := FromGin(c)
	if d == nil {
		return
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return
	}
	path := rawURL
	if idx := strings.IndexAny(path, "?#"); idx >= 0 {
		path = path[:idx]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.EffectiveURL = rawURL
	d.EffectivePath = path
}

func SetRoute(c *gin.Context, route *internalrouting.PathRouteContext) {
	d := FromGin(c)
	if d == nil || route == nil {
		return
	}
	snapshot := RouteSnapshot{
		RoutePath: strings.TrimSpace(route.RoutePath),
		Group:     strings.TrimSpace(route.Group),
		Fallback:  strings.TrimSpace(route.Fallback),
	}
	if route.CcSwitch != nil {
		snapshot.CcSwitch = &CcSwitchSnapshot{
			ConfigID:             strings.TrimSpace(route.CcSwitch.ConfigID),
			ClientType:           strings.TrimSpace(route.CcSwitch.ClientType),
			DefaultModel:         strings.TrimSpace(route.CcSwitch.DefaultModel),
			RoutePath:            strings.TrimSpace(route.CcSwitch.RoutePath),
			EndpointPath:         strings.TrimSpace(route.CcSwitch.EndpointPath),
			AllowedChannelGroups: append([]string(nil), route.CcSwitch.AllowedChannelGroups...),
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Route = &snapshot
}

func SetAuth(c *gin.Context, provider, apiKey, apiKeyID, apiKeyName string) {
	d := FromGin(c)
	if d == nil {
		return
	}
	auth := &AuthSnapshot{
		Provider:   strings.TrimSpace(provider),
		APIKey:     util.HideAPIKey(strings.TrimSpace(apiKey)),
		APIKeyID:   strings.TrimSpace(apiKeyID),
		APIKeyName: strings.TrimSpace(apiKeyName),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Auth = auth
}

func SetQuotaLimits(c *gin.Context, quota QuotaSnapshot) {
	d := FromGin(c)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Quota == nil {
		d.Quota = &QuotaSnapshot{}
	}
	d.Quota.DailyLimit = quota.DailyLimit
	d.Quota.TotalQuota = quota.TotalQuota
	d.Quota.ConcurrencyLimit = quota.ConcurrencyLimit
	d.Quota.RPMLimit = quota.RPMLimit
	d.Quota.TPMLimit = quota.TPMLimit
	d.Quota.SpendingLimit = quota.SpendingLimit
	d.Quota.DailySpendingLimit = quota.DailySpendingLimit
}

func SetQuotaRejection(c *gin.Context, rejectedBy string, limit, current float64, code, typ, message string) {
	d := FromGin(c)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Quota == nil {
		d.Quota = &QuotaSnapshot{}
	}
	d.Quota.Rejected = true
	d.Quota.RejectedBy = strings.TrimSpace(rejectedBy)
	d.Quota.Limit = limit
	d.Quota.Current = current
	d.Quota.ErrorCode = strings.TrimSpace(code)
	d.Quota.ErrorType = strings.TrimSpace(typ)
	d.Quota.ErrorMessage = strings.TrimSpace(message)
	setResponseLocked(d, http.StatusTooManyRequests, code, typ, message, "local_quota")
}

func SetLocalError(c *gin.Context, status int, source, code, typ, message string) {
	d := FromGin(c)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	setResponseLocked(d, status, code, typ, message, source)
}

func SetUpstreamRequest(ctx context.Context, attempt int, provider, authID, authLabel, rawURL, method string) {
	d := FromContext(ctx)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Upstream == nil {
		d.Upstream = &UpstreamSnapshot{}
	}
	d.Upstream.Attempt = attempt
	d.Upstream.Provider = strings.TrimSpace(provider)
	d.Upstream.AuthID = strings.TrimSpace(authID)
	d.Upstream.AuthLabel = strings.TrimSpace(authLabel)
	d.Upstream.URL = strings.TrimSpace(rawURL)
	d.Upstream.Method = strings.TrimSpace(method)
}

func SetUpstreamResponse(ctx context.Context, status int) {
	d := FromContext(ctx)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Upstream == nil {
		d.Upstream = &UpstreamSnapshot{}
	}
	d.Upstream.Status = status
}

func SetUpstreamError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	d := FromContext(ctx)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Upstream == nil {
		d.Upstream = &UpstreamSnapshot{}
	}
	d.Upstream.ErrorMessage = strings.TrimSpace(err.Error())
}

func SetEgress(c *gin.Context, egress map[string]any) {
	d := FromGin(c)
	if d == nil || len(egress) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Egress = cloneAnyMap(egress)
}

func RecordResponse(c *gin.Context, status int, body []byte) {
	d := FromGin(c)
	if d == nil {
		return
	}
	code, typ, message := parseErrorBody(body)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Response != nil {
		if d.Response.Status == 0 {
			d.Response.Status = status
		}
		if d.Response.ErrorCode == "" {
			d.Response.ErrorCode = code
		}
		if d.Response.ErrorType == "" {
			d.Response.ErrorType = typ
		}
		if d.Response.ErrorMessage == "" {
			d.Response.ErrorMessage = message
		}
		return
	}
	source := ""
	if status >= http.StatusBadRequest && d.Upstream != nil {
		if d.Upstream.Status > 0 || d.Upstream.ErrorMessage != "" {
			source = "upstream"
		}
	}
	d.Response = &ResponseSnapshot{
		Status:       status,
		ErrorCode:    code,
		ErrorType:    typ,
		ErrorMessage: message,
		Source:       source,
	}
}

func RecordBody(c *gin.Context, body []byte, redacted bool, reason string) *BodySnapshot {
	d := FromGin(c)
	if d == nil {
		return nil
	}
	contentType := ""
	contentLength := int64(0)
	if c != nil && c.Request != nil {
		contentType = c.Request.Header.Get("Content-Type")
		contentLength = c.Request.ContentLength
	}
	snapshot := summarizeBody(body, contentType, contentLength, redacted, reason)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Body = &snapshot
	return &snapshot
}

func ShouldRedactErrorOnlyBody(c *gin.Context, status int) bool {
	d := FromGin(c)
	if d == nil || status < 400 || status >= 500 {
		return false
	}
	snapshot := d.Snapshot()
	if snapshot.Response == nil {
		return false
	}
	return strings.HasPrefix(snapshot.Response.Source, "local_")
}

func (d *RequestDiagnostic) Snapshot() Snapshot {
	if d == nil {
		return Snapshot{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return Snapshot{
		RequestID:      d.RequestID,
		Method:         d.Method,
		OriginalURL:    d.OriginalURL,
		EffectiveURL:   d.EffectiveURL,
		OriginalPath:   d.OriginalPath,
		EffectivePath:  d.EffectivePath,
		Host:           d.Host,
		RemoteAddr:     d.RemoteAddr,
		ClientIP:       d.ClientIP,
		ClientIPSource: d.ClientIPSource,
		UserAgent:      d.UserAgent,
		ContentType:    d.ContentType,
		ContentLength:  d.ContentLength,
		Route:          cloneRoute(d.Route),
		Auth:           cloneAuth(d.Auth),
		Quota:          cloneQuota(d.Quota),
		Upstream:       cloneUpstream(d.Upstream),
		Egress:         cloneAnyMap(d.Egress),
		Response:       cloneResponse(d.Response),
		Body:           cloneBody(d.Body),
	}
}

func (s Snapshot) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

func (s Snapshot) IsZero() bool {
	return s.RequestID == "" &&
		s.Method == "" &&
		s.OriginalURL == "" &&
		s.EffectiveURL == "" &&
		s.OriginalPath == "" &&
		s.EffectivePath == "" &&
		s.Host == "" &&
		s.RemoteAddr == "" &&
		s.ClientIP == "" &&
		s.ClientIPSource == "" &&
		s.UserAgent == "" &&
		s.ContentType == "" &&
		s.ContentLength == 0 &&
		s.Route == nil &&
		s.Auth == nil &&
		s.Quota == nil &&
		s.Upstream == nil &&
		s.Egress == nil &&
		s.Response == nil &&
		s.Body == nil
}

func FormatBodySummary(snapshot *BodySnapshot) []byte {
	if snapshot == nil {
		return nil
	}
	data, err := json.MarshalIndent(map[string]any{
		"note":    "request body redacted for local gateway error log",
		"summary": snapshot,
	}, "", "  ")
	if err != nil {
		return []byte("[request body redacted]\n")
	}
	return append(data, '\n')
}

func requestURL(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	rawQuery := util.MaskSensitiveQuery(req.URL.RawQuery)
	if rawQuery == "" {
		return req.URL.Path
	}
	return req.URL.Path + "?" + rawQuery
}

func setResponseLocked(d *RequestDiagnostic, status int, code, typ, message, source string) {
	if d.Response == nil {
		d.Response = &ResponseSnapshot{}
	}
	d.Response.Status = status
	d.Response.ErrorCode = strings.TrimSpace(code)
	d.Response.ErrorType = strings.TrimSpace(typ)
	d.Response.ErrorMessage = strings.TrimSpace(message)
	d.Response.Source = strings.TrimSpace(source)
}

func parseErrorBody(body []byte) (string, string, string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", "", ""
	}
	errValue, ok := root["error"]
	if !ok {
		return "", "", ""
	}
	switch value := errValue.(type) {
	case string:
		return "", "", strings.TrimSpace(value)
	case map[string]any:
		return stringValue(value["code"]), stringValue(value["type"]), stringValue(value["message"])
	default:
		return "", "", ""
	}
}

func summarizeBody(body []byte, contentType string, contentLength int64, redacted bool, reason string) BodySnapshot {
	snapshot := BodySnapshot{
		ContentType:   strings.TrimSpace(contentType),
		ContentLength: contentLength,
		CapturedBytes: len(body),
		Redacted:      redacted,
		RedactedBy:    strings.TrimSpace(reason),
	}
	if len(body) == 0 || !strings.Contains(strings.ToLower(contentType), "json") {
		return snapshot
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return snapshot
	}
	snapshot.Model = stringValue(root["model"])
	if stream, ok := root["stream"].(bool); ok {
		snapshot.Stream = &stream
	}
	if items, ok := root["input"].([]any); ok {
		snapshot.InputItems = len(items)
	}
	if messages, ok := root["messages"].([]any); ok {
		snapshot.Messages = len(messages)
	}
	return snapshot
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

func cloneRoute(in *RouteSnapshot) *RouteSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	if in.CcSwitch != nil {
		cc := *in.CcSwitch
		cc.AllowedChannelGroups = append([]string(nil), in.CcSwitch.AllowedChannelGroups...)
		out.CcSwitch = &cc
	}
	return &out
}

func cloneAuth(in *AuthSnapshot) *AuthSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneQuota(in *QuotaSnapshot) *QuotaSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneUpstream(in *UpstreamSnapshot) *UpstreamSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneResponse(in *ResponseSnapshot) *ResponseSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneBody(in *BodySnapshot) *BodySnapshot {
	if in == nil {
		return nil
	}
	out := *in
	if in.Stream != nil {
		stream := *in.Stream
		out.Stream = &stream
	}
	return &out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
