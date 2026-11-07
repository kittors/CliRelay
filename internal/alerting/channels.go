package alerting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

// Delivery statuses.
const (
	DeliverySent   = "sent"
	DeliveryFailed = "failed"
)

// telegramAPIBase is where Telegram bots are called. A variable so the tests
// can aim it at a local server; no setting reaches it.
var telegramAPIBase = "https://api.telegram.org"

const (
	// responseLimit bounds how much of a receiver's answer is read. The answer is
	// only ever inspected, never stored or shown.
	responseLimit = 4 << 10

	// Chat channels cap a text message; stay under their limits.
	telegramTextLimit = 4000
	feishuTextLimit   = 8000

	userAgent = "CliRelay-Alerts/1"
)

// sendError is a delivery failure. Its text is safe to show and to store: it
// never contains the address, a token, or anything the receiver answered with
// that was not first reduced to a short reason.
type sendError struct{ reason string }

func (e *sendError) Error() string { return e.reason }

func failure(format string, args ...any) error {
	return &sendError{reason: fmt.Sprintf(format, args...)}
}

// buildRequest makes the HTTP request that delivers msg to channel.
func buildRequest(ctx context.Context, channel Channel, msg Message, now time.Time) (*http.Request, error) {
	switch channel.Kind {
	case KindWebhook:
		return webhookRequest(ctx, channel, msg, now)
	case KindFeishu:
		return feishuRequest(ctx, channel, msg, now)
	case KindTelegram:
		return telegramRequest(ctx, channel, msg)
	default:
		return nil, failure("unknown channel kind")
	}
}

func newJSONRequest(ctx context.Context, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// url.Error carries the address; report the fact, not the error.
		return nil, failure("the address is not usable")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// webhookRequest posts the message as JSON. With a secret the request is signed
// so a receiver can tell it came from here and was not replayed:
//
//	X-CliRelay-Timestamp: <unix seconds>
//	X-CliRelay-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">
func webhookRequest(ctx context.Context, channel Channel, msg Message, now time.Time) (*http.Request, error) {
	msg.Version = payloadVersion
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, failure("the message could not be encoded")
	}
	req, err := newJSONRequest(ctx, channel.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-CliRelay-Event", msg.Rule)
	req.Header.Set("X-CliRelay-Delivery", msg.ID)
	if channel.Secret != "" {
		timestamp := strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(channel.Secret))
		mac.Write([]byte(timestamp))
		mac.Write([]byte("."))
		mac.Write(body)
		req.Header.Set("X-CliRelay-Timestamp", timestamp)
		req.Header.Set("X-CliRelay-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return req, nil
}

// feishuRequest posts a text message to a Feishu or Lark custom bot. When the
// bot has "signature verification" on, the body carries the timestamp and the
// signature Feishu documents: Base64(HMAC-SHA256(key = "<timestamp>\n<secret>",
// data = empty)).
func feishuRequest(ctx context.Context, channel Channel, msg Message, now time.Time) (*http.Request, error) {
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": truncateRunes(msg.Plain(), feishuTextLimit)},
	}
	if channel.Secret != "" {
		timestamp := strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(timestamp+"\n"+channel.Secret))
		payload["timestamp"] = timestamp
		payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, failure("the message could not be encoded")
	}
	return newJSONRequest(ctx, channel.URL, body)
}

// telegramRequest calls the Bot API's sendMessage. The text is sent as plain
// text, not Markdown or HTML: account and key names are free text, and one
// stray underscore or angle bracket would make Telegram refuse the whole
// message.
func telegramRequest(ctx context.Context, channel Channel, msg Message) (*http.Request, error) {
	body, err := json.Marshal(map[string]any{
		"chat_id":                  channel.ChatID,
		"text":                     truncateRunes(msg.Plain(), telegramTextLimit),
		"disable_web_page_preview": true,
	})
	if err != nil {
		return nil, failure("the message could not be encoded")
	}
	// The token is validated to hold no character that could add a path segment
	// or a query (see ValidateTelegramToken).
	return newJSONRequest(ctx, telegramAPIBase+"/bot"+channel.BotToken+"/sendMessage", body)
}

// checkResponse decides from the receiver's answer whether the message was
// accepted. Chat services answer 200 even for a refusal and say so in the body,
// so for them the status alone proves nothing.
func checkResponse(kind string, status int, body []byte) error {
	switch kind {
	case KindFeishu:
		return checkFeishu(status, body)
	case KindTelegram:
		return checkTelegram(status, body)
	default:
		if status >= 200 && status < 300 {
			return nil
		}
		return failure("the receiver answered HTTP %d", status)
	}
}

func checkFeishu(status int, body []byte) error {
	var answer struct {
		Code       *int   `json:"code"`
		StatusCode *int   `json:"StatusCode"`
		Msg        string `json:"msg"`
		StatusMsg  string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || (answer.Code == nil && answer.StatusCode == nil) {
		if status >= 200 && status < 300 {
			return failure("Feishu answered something that is not a bot reply")
		}
		return failure("Feishu answered HTTP %d", status)
	}
	code, msg := 0, answer.Msg
	if answer.Code != nil {
		code = *answer.Code
	} else {
		code, msg = *answer.StatusCode, answer.StatusMsg
	}
	if code == 0 {
		return nil
	}
	// Feishu's explanation ("sign match fail", "keywords not match", "ip not in
	// whitelist") is what the operator needs and holds nothing of ours.
	return failure("Feishu refused the message (code %d): %s", code, cleanText(truncateRunes(msg, 160)))
}

func checkTelegram(status int, body []byte) error {
	var answer struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return failure("Telegram answered HTTP %d", status)
	}
	if answer.OK {
		return nil
	}
	return failure("Telegram refused the message (%d): %s", answer.ErrorCode, cleanText(truncateRunes(answer.Description, 160)))
}

// transportReason reduces a failed request to a reason that is safe to show.
// net/http's own error text contains the full URL, which for a chat channel is
// the credential, so it is never passed on.
func transportReason(err error) string {
	var (
		dnsErr     *net.DNSError
		netErr     net.Error
		unknownCA  x509.UnknownAuthorityError
		certErr    x509.CertificateInvalidError
		hostErr    x509.HostnameError
		sendFailed *sendError
	)
	switch {
	case errors.As(err, &sendFailed):
		return sendFailed.reason
	case errors.Is(err, errBlockedAddress):
		return errBlockedAddress.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the receiver did not answer in time"
	case errors.Is(err, context.Canceled):
		return "the delivery was cancelled"
	case errors.As(err, &dnsErr):
		return "the host name could not be resolved"
	case errors.As(err, &unknownCA), errors.As(err, &certErr), errors.As(err, &hostErr):
		return "the receiver's TLS certificate is not valid"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "the receiver did not answer in time"
	default:
		return "the request failed"
	}
}

// readLimited reads up to responseLimit bytes of a response body.
func readLimited(r io.Reader) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, responseLimit))
	return bytes.TrimSpace(data)
}
