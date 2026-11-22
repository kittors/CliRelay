package billing

import (
	"context"
	"crypto/md5" //nolint:gosec // XunhuPay protocol mandates MD5 request/callback signatures.
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// XunhuClient implements the XunhuPay (虎皮椒) v1.1 HTTP protocol for order
// creation, order status polling, and credential/connectivity verification.
type XunhuClient struct {
	HTTPClient *http.Client
}

func NewXunhuClient(httpClient *http.Client) *XunhuClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &XunhuClient{HTTPClient: httpClient}
}

// SignXunhu computes the XunhuPay MD5 signature: sort all non-empty parameters
// except "hash" by ASCII key order, join as k=v with "&", append appSecret,
// and take lowercase hex MD5.
func SignXunhu(params map[string]string, appSecret string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "hash" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	b.WriteString(appSecret)
	sum := md5.Sum([]byte(b.String())) //nolint:gosec // XunhuPay protocol mandates MD5 signatures.
	return hex.EncodeToString(sum[:])
}

// VerifyXunhuSignature verifies a XunhuPay callback/response signature in constant time.
func VerifyXunhuSignature(params map[string]string, appSecret string) bool {
	if strings.TrimSpace(appSecret) == "" {
		return false
	}
	got := strings.ToLower(strings.TrimSpace(params["hash"]))
	if len(got) != 32 {
		return false
	}
	want := SignXunhu(params, appSecret)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// XunhuCreateRequest carries the parameters for XunhuPay payment/do.html.
type XunhuCreateRequest struct {
	GatewayURL   string
	AppID        string
	AppSecret    string
	Channel      string
	TradeOrderID string
	AmountCents  int
	Title        string
	NotifyURL    string
	ReturnURL    string
	WapURL       string
	WapName      string
}

type XunhuCreateResponse struct {
	OpenOrderID string
	QRCodeURL   string
	PayURL      string
}

// CreatePayment submits a unified payment order to XunhuPay and returns the QR code and pay URLs.
func (c *XunhuClient) CreatePayment(ctx context.Context, req XunhuCreateRequest) (XunhuCreateResponse, error) {
	gateway := strings.TrimSpace(req.GatewayURL)
	if gateway == "" {
		gateway = DefaultXunhuGatewayURL
	}
	if err := validateHTTPURL("xunhu_gateway_url", gateway); err != nil {
		return XunhuCreateResponse{}, err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return XunhuCreateResponse{}, err
	}
	params := map[string]string{
		"version":        "1.1",
		"appid":          strings.TrimSpace(req.AppID),
		"trade_order_id": strings.TrimSpace(req.TradeOrderID),
		"total_fee":      FormatCNYYuan(req.AmountCents),
		"title":          truncateTitle(req.Title, 120),
		"time":           strconv.FormatInt(time.Now().Unix(), 10),
		"notify_url":     strings.TrimSpace(req.NotifyURL),
		"return_url":     strings.TrimSpace(req.ReturnURL),
		"nonce_str":      nonce,
		"plugins":        "clirelay",
	}
	if req.Channel == ChannelWeChat {
		params["type"] = "WAP"
		if strings.TrimSpace(req.WapURL) != "" {
			params["wap_url"] = strings.TrimSpace(req.WapURL)
		}
		if strings.TrimSpace(req.WapName) != "" {
			params["wap_name"] = strings.TrimSpace(req.WapName)
		}
	}
	params["hash"] = SignXunhu(params, req.AppSecret)

	raw, err := c.postForm(ctx, gateway, params)
	if err != nil {
		return XunhuCreateResponse{}, err
	}
	var resp struct {
		ErrCode   int    `json:"errcode"`
		ErrMsg    string `json:"errmsg"`
		OpenID    string `json:"openid"`
		URLQRCode string `json:"url_qrcode"`
		URL       string `json:"url"`
		Hash      string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return XunhuCreateResponse{}, fmt.Errorf("%w: invalid xunhupay response", ErrProviderFailed)
	}
	if resp.ErrCode != 0 {
		msg := strings.TrimSpace(resp.ErrMsg)
		if msg == "" {
			msg = fmt.Sprintf("errcode %d", resp.ErrCode)
		}
		return XunhuCreateResponse{}, fmt.Errorf("%w: xunhupay: %s", ErrProviderFailed, msg)
	}
	if strings.TrimSpace(resp.URLQRCode) == "" && strings.TrimSpace(resp.URL) == "" {
		return XunhuCreateResponse{}, fmt.Errorf("%w: xunhupay returned neither qrcode nor pay url", ErrProviderFailed)
	}
	return XunhuCreateResponse{
		OpenOrderID: strings.TrimSpace(resp.OpenID),
		QRCodeURL:   strings.TrimSpace(resp.URLQRCode),
		PayURL:      strings.TrimSpace(resp.URL),
	}, nil
}

type XunhuQueryRequest struct {
	QueryURL     string
	AppID        string
	AppSecret    string
	TradeOrderID string
}

type XunhuQueryResponse struct {
	Status      string // "OD" (paid), "WP" (waiting), "CD" (closed)
	OpenOrderID string
	AmountCents int
}

// QueryOrder queries XunhuPay payment/query.html for the latest order status.
func (c *XunhuClient) QueryOrder(ctx context.Context, req XunhuQueryRequest) (XunhuQueryResponse, error) {
	endpoint := strings.TrimSpace(req.QueryURL)
	if endpoint == "" {
		endpoint = DefaultXunhuQueryURL
	}
	if err := validateHTTPURL("xunhu_query_url", endpoint); err != nil {
		return XunhuQueryResponse{}, err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return XunhuQueryResponse{}, err
	}
	params := map[string]string{
		"appid":           strings.TrimSpace(req.AppID),
		"out_trade_order": strings.TrimSpace(req.TradeOrderID),
		"time":            strconv.FormatInt(time.Now().Unix(), 10),
		"nonce_str":       nonce,
	}
	params["hash"] = SignXunhu(params, req.AppSecret)

	raw, err := c.postForm(ctx, endpoint, params)
	if err != nil {
		return XunhuQueryResponse{}, err
	}
	var envelope struct {
		ErrCode int             `json:"errcode"`
		ErrMsg  string          `json:"errmsg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return XunhuQueryResponse{}, fmt.Errorf("%w: invalid xunhupay query response", ErrProviderFailed)
	}
	if envelope.ErrCode != 0 {
		msg := strings.TrimSpace(envelope.ErrMsg)
		if msg == "" {
			msg = fmt.Sprintf("errcode %d", envelope.ErrCode)
		}
		return XunhuQueryResponse{}, fmt.Errorf("%w: xunhupay query: %s", ErrProviderFailed, msg)
	}
	var data struct {
		Status      string `json:"status"`
		OpenOrderID string `json:"open_order_id"`
		TotalFee    any    `json:"total_fee"`
	}
	if len(envelope.Data) > 0 {
		_ = json.Unmarshal(envelope.Data, &data)
	}
	cents := 0
	if data.TotalFee != nil {
		if parsed, parseErr := ParseCNYCents(fmt.Sprint(data.TotalFee)); parseErr == nil {
			cents = parsed
		}
	}
	return XunhuQueryResponse{
		Status:      strings.ToUpper(strings.TrimSpace(data.Status)),
		OpenOrderID: strings.TrimSpace(data.OpenOrderID),
		AmountCents: cents,
	}, nil
}

// TestChannel probes XunhuPay's query endpoint with a synthetic order ID to verify
// that the gateway is reachable and the AppID/AppSecret signature is accepted.
func (c *XunhuClient) TestChannel(ctx context.Context, queryURL, appID, appSecret string) error {
	endpoint := strings.TrimSpace(queryURL)
	if endpoint == "" {
		endpoint = DefaultXunhuQueryURL
	}
	if err := validateHTTPURL("xunhu_query_url", endpoint); err != nil {
		return err
	}
	nonce, err := randomHex(12)
	if err != nil {
		return err
	}
	params := map[string]string{
		"appid":           strings.TrimSpace(appID),
		"out_trade_order": "CPTEST" + nonce,
		"time":            strconv.FormatInt(time.Now().Unix(), 10),
		"nonce_str":       nonce,
	}
	params["hash"] = SignXunhu(params, appSecret)
	raw, err := c.postForm(ctx, endpoint, params)
	if err != nil {
		return err
	}
	var resp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("%w: unexpected response from xunhupay gateway", ErrProviderFailed)
	}
	// XunhuPay returns errcode 0 (empty data) or "order not found" (commonly errcode 404/500 with
	// order-missing message) when credentials and signature are valid. Signature or appid errors
	// explicitly mention hash/sign/appid/secret.
	if resp.ErrCode == 0 {
		return nil
	}
	lower := strings.ToLower(resp.ErrMsg)
	if strings.Contains(lower, "hash") || strings.Contains(lower, "sign") ||
		strings.Contains(lower, "appid") || strings.Contains(lower, "secret") ||
		strings.Contains(resp.ErrMsg, "签名") || strings.Contains(resp.ErrMsg, "密钥") ||
		strings.Contains(resp.ErrMsg, "商户") {
		return fmt.Errorf("%w: xunhupay rejected credentials: %s", ErrProviderFailed, strings.TrimSpace(resp.ErrMsg))
	}
	return nil
}

func (c *XunhuClient) postForm(ctx context.Context, endpoint string, params map[string]string) ([]byte, error) {
	form := url.Values{}
	for k, v := range params {
		if v != "" {
			form.Set(k, v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrProviderFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: request xunhupay: %v", ErrProviderFailed, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read xunhupay response: %v", ErrProviderFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: xunhupay http %d", ErrProviderFailed, resp.StatusCode)
	}
	return body, nil
}

// FormatCNYYuan renders integer cents as "30.00".
func FormatCNYYuan(cents int) string {
	return fmt.Sprintf("%.2f", float64(cents)/100.0)
}

// ParseCNYCents parses a decimal yuan string (e.g. "30.00" or "0.01") into exact integer cents.
func ParseCNYCents(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("invalid amount %q", raw)
	}
	return int(math.Round(f * 100)), nil
}

func validateHTTPURL(field, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(u.Host) == "" {
		return fieldErr(field, "must be a valid http or https URL")
	}
	return nil
}

func truncateTitle(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "CliRelay Order"
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
