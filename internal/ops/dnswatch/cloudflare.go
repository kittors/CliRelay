package dnswatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	userAgent           = "clirelay-dnswatch"
	cfAttempts          = 3
	cfRequestTimeout    = 10 * time.Second
	defaultRetryBackoff = time.Second
	maxAPIResponseBytes = 1 << 20
	maxListPages        = 20
	redactedToken       = "[redacted]"
)

// dnsRecord is the part of a Cloudflare DNS record dnswatch reads and writes.
type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// apiMessage is one entry of a Cloudflare "errors" list.
type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiEnvelope struct {
	Success    bool            `json:"success"`
	Errors     []apiMessage    `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

// apiError describes a failed Cloudflare call. Status is 0 when the request
// never produced an HTTP response.
type apiError struct {
	Method   string
	Path     string
	Status   int
	Messages []apiMessage
	Err      error
}

func (e *apiError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cloudflare %s %s", e.Method, e.Path)
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	for _, m := range e.Messages {
		fmt.Fprintf(&b, ": [%d] %s", m.Code, m.Message)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *apiError) Unwrap() error { return e.Err }

// retryable reports whether repeating the call can help: transport failures,
// rate limiting and server-side errors. Other 4xx answers (bad token, missing
// permission, invalid record) will not change on retry.
func (e *apiError) retryable() bool {
	return e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// redactedError keeps the error chain (for errors.Is) while its message is
// guaranteed free of the API token.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// cloudflareClient is a minimal client for the zone DNS records API.
type cloudflareClient struct {
	baseURL    string
	zoneID     string
	token      string
	httpClient *http.Client
	backoff    time.Duration
}

func newCloudflareClient(cfg CloudflareConfig, token string, httpClient *http.Client, backoff time.Duration) *cloudflareClient {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	return &cloudflareClient{
		baseURL:    strings.TrimRight(cfg.APIBaseURL, "/"),
		zoneID:     cfg.ZoneID,
		token:      token,
		httpClient: httpClient,
		backoff:    backoff,
	}
}

func (c *cloudflareClient) recordsPath() string {
	return "/zones/" + url.PathEscape(c.zoneID) + "/dns_records"
}

// listARecords returns the A records named exactly name.
func (c *cloudflareClient) listARecords(ctx context.Context, name string) ([]dnsRecord, error) {
	var records []dnsRecord
	for page := 1; ; page++ {
		query := url.Values{
			"type":     {"A"},
			"name":     {name},
			"per_page": {"100"},
			"page":     {strconv.Itoa(page)},
		}
		envelope, err := c.do(ctx, http.MethodGet, c.recordsPath(), query, nil)
		if err != nil {
			return nil, err
		}
		var batch []dnsRecord
		if err := json.Unmarshal(envelope.Result, &batch); err != nil {
			return nil, fmt.Errorf("cloudflare list %s: decode records: %w", name, err)
		}
		records = append(records, batch...)
		if envelope.ResultInfo == nil || page >= envelope.ResultInfo.TotalPages || page >= maxListPages {
			return records, nil
		}
	}
}

// createARecord publishes ip under name, always unproxied: dnswatch exists to
// steer clients straight to the nodes, which an orange-cloud record would hide.
func (c *cloudflareClient) createARecord(ctx context.Context, name, ip string, ttl int) (dnsRecord, error) {
	payload := dnsRecord{Type: "A", Name: name, Content: ip, TTL: ttl, Proxied: false}
	envelope, err := c.do(ctx, http.MethodPost, c.recordsPath(), nil, payload)
	if err != nil {
		return dnsRecord{}, err
	}
	var created dnsRecord
	if err := json.Unmarshal(envelope.Result, &created); err != nil {
		return dnsRecord{}, fmt.Errorf("cloudflare create %s %s: decode record: %w", name, ip, err)
	}
	return created, nil
}

// deleteRecord removes the record with the given id.
func (c *cloudflareClient) deleteRecord(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, c.recordsPath()+"/"+url.PathEscape(id), nil, nil)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		// The goal of a delete is absence: a record already removed by an
		// earlier attempt whose reply was lost, or by hand, counts as done.
		return nil
	}
	return err
}

// do sends one API call, retrying transient failures with exponential backoff.
func (c *cloudflareClient) do(ctx context.Context, method, path string, query url.Values, payload any) (*apiEnvelope, error) {
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("cloudflare %s %s: encode request: %w", method, path, err)
		}
		body = encoded
	}
	var lastErr error
	for attempt := 1; attempt <= cfAttempts; attempt++ {
		envelope, err := c.doOnce(ctx, method, path, query, body)
		if err == nil {
			return envelope, nil
		}
		lastErr = err
		var apiErr *apiError
		if !errors.As(err, &apiErr) || !apiErr.retryable() || attempt == cfAttempts || ctx.Err() != nil {
			break
		}
		if sleepContext(ctx, c.backoff<<(attempt-1)) != nil {
			break
		}
	}
	return nil, lastErr
}

func (c *cloudflareClient) doOnce(ctx context.Context, method, path string, query url.Values, body []byte) (*apiEnvelope, error) {
	ctx, cancel := context.WithTimeout(ctx, cfRequestTimeout)
	defer cancel()

	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, &apiError{Method: method, Path: path, Err: c.redactError(err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &apiError{Method: method, Path: path, Err: c.redactError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		// A truncated body is a transport problem, so leave Status at 0 to
		// keep it retryable.
		return nil, &apiError{Method: method, Path: path, Err: c.redactError(err)}
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &apiError{
			Method: method,
			Path:   path,
			Status: resp.StatusCode,
			Err:    &redactedError{msg: "unexpected response: " + c.redact(snippet(raw)), cause: err},
		}
	}
	if resp.StatusCode/100 != 2 || !envelope.Success {
		status := resp.StatusCode
		if status/100 == 2 {
			// success=false inside a 2xx reply: report it without making it
			// look like a transport failure.
			status = http.StatusBadRequest
		}
		messages := make([]apiMessage, 0, len(envelope.Errors))
		for _, m := range envelope.Errors {
			messages = append(messages, apiMessage{Code: m.Code, Message: c.redact(m.Message)})
		}
		return nil, &apiError{Method: method, Path: path, Status: status, Messages: messages}
	}
	return &envelope, nil
}

// redact strips the API token from text headed for logs or alerts. Cloudflare
// has no reason to echo it, but an intercepting proxy or a debugging
// endpoint might, and the token must never reach the journal.
func (c *cloudflareClient) redact(text string) string {
	if c.token == "" {
		return text
	}
	return strings.ReplaceAll(text, c.token, redactedToken)
}

func (c *cloudflareClient) redactError(err error) error {
	return &redactedError{msg: c.redact(err.Error()), cause: err}
}

func snippet(raw []byte) string {
	const limit = 200
	text := strings.TrimSpace(string(raw))
	if len(text) > limit {
		text = text[:limit] + "..."
	}
	return text
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
