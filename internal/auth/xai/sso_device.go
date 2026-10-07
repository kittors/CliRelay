package xai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Turning a Grok web session into Grok Build OAuth tokens.
//
// accounts.x.ai keeps a signed-in browser session in the sso (and sso-rw)
// cookie. xAI's OAuth server offers the device authorization grant for the
// Grok CLI client, so the account owner's session can do what they would do by
// hand: open the verification page, confirm the user code and approve it. The
// token endpoint then hands out the same token set the browser login produces.
// The session cookie is only ever sent to x.ai hosts over HTTPS.

const (
	ssoAccountsURL   = "https://accounts.x.ai/"
	ssoDeviceCodeURL = Issuer + "/oauth2/device/code"
	ssoVerifyURL     = Issuer + "/oauth2/device/verify"
	ssoApproveURL    = Issuer + "/oauth2/device/approve"
	ssoTokenURL      = Issuer + "/oauth2/token"

	ssoMaxRedirects   = 8
	ssoMaxBody        = 2 << 20
	ssoMaxTokenLength = 16 << 10
	// ssoPollWindow bounds how long the token endpoint is polled after the
	// approval went through; the token is normally ready on the first poll.
	ssoPollWindow = 75 * time.Second
	ssoUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

var (
	// ErrSSOInvalid: accounts.x.ai does not accept the session.
	ErrSSOInvalid = errors.New("grok web session is invalid or expired")
	// ErrSSODenied: xAI declined or expired the device authorization.
	ErrSSODenied = errors.New("xai declined the device authorization")
)

// ssoSleep waits between token polls; tests make it instant.
var ssoSleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// NormalizeSSOToken extracts the session token from what operators paste: the
// bare value, or a cookie header containing sso=… or sso-rw=…. It returns ""
// for anything that cannot be one.
func NormalizeSSOToken(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}
	if strings.Contains(value, "=") && (strings.Contains(value, ";") || strings.HasPrefix(strings.ToLower(value), "sso")) {
		for _, part := range strings.Split(value, ";") {
			name, token, found := strings.Cut(strings.TrimSpace(part), "=")
			if !found {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "sso", "sso-rw":
				return sanitizeSSOToken(token)
			}
		}
		if strings.Contains(value, ";") {
			return ""
		}
	}
	return sanitizeSSOToken(value)
}

func sanitizeSSOToken(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" || len(value) > ssoMaxTokenLength || strings.ContainsAny(value, " \t\r\n\x00;,") {
		return ""
	}
	return value
}

func ssoHostAllowed(u *url.URL) bool {
	if u == nil || u.User != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

// ConvertSSOToTokens runs the device authorization grant with a Grok web
// session and returns the tokens and the token endpoint they refresh against.
func (a *XAIAuth) ConvertSSOToTokens(ctx context.Context, ssoToken string) (*TokenData, string, error) {
	token := NormalizeSSOToken(ssoToken)
	if token == "" {
		return nil, "", ErrSSOInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, "", err
	}
	for _, raw := range []string{ssoAccountsURL, Issuer + "/"} {
		target, _ := url.Parse(raw)
		jar.SetCookies(target, []*http.Cookie{
			{Name: "sso", Value: token, Path: "/", Secure: true, HttpOnly: true},
			{Name: "sso-rw", Value: token, Path: "/", Secure: true, HttpOnly: true},
		})
	}
	client := *a.httpClient
	client.Jar = nil
	// Redirects are followed by hand so each hop's cookies are captured and
	// the session never leaves x.ai.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	flow := &ssoFlow{client: &client, jar: jar}

	status, finalURL, _, err := flow.do(ctx, http.MethodGet, ssoAccountsURL, nil)
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusUnauthorized || strings.Contains(finalURL, "sign-in") || strings.Contains(finalURL, "sign-up") {
		return nil, "", ErrSSOInvalid
	}
	if status >= 400 {
		return nil, "", fmt.Errorf("check the grok web session: status %d", status)
	}

	status, _, body, err := flow.do(ctx, http.MethodPost, ssoDeviceCodeURL, url.Values{
		"client_id": {ClientID},
		"scope":     {Scope},
	})
	if err != nil {
		return nil, "", err
	}
	if status < 200 || status >= 300 {
		return nil, "", fmt.Errorf("start the device authorization: status %d", status)
	}
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err = json.Unmarshal(body, &device); err != nil {
		return nil, "", fmt.Errorf("parse the device authorization: %w", err)
	}
	verification, errParse := url.Parse(strings.TrimSpace(device.VerificationURIComplete))
	if device.DeviceCode == "" || device.UserCode == "" || errParse != nil || !ssoHostAllowed(verification) {
		return nil, "", errors.New("the device authorization response is incomplete")
	}

	if status, _, _, err = flow.do(ctx, http.MethodGet, verification.String(), nil); err != nil {
		return nil, "", err
	} else if status >= 400 {
		return nil, "", fmt.Errorf("open the verification page: status %d", status)
	}
	status, finalURL, _, err = flow.do(ctx, http.MethodPost, ssoVerifyURL, url.Values{"user_code": {device.UserCode}})
	if err != nil {
		return nil, "", err
	}
	if status >= 400 || !strings.Contains(finalURL, "consent") {
		return nil, "", fmt.Errorf("confirm the user code: status %d", status)
	}
	status, finalURL, _, err = flow.do(ctx, http.MethodPost, ssoApproveURL, url.Values{
		"user_code":      {device.UserCode},
		"action":         {"allow"},
		"principal_type": {"User"},
		"principal_id":   {""},
	})
	if err != nil {
		return nil, "", err
	}
	if status >= 400 || !strings.Contains(finalURL, "done") {
		return nil, "", fmt.Errorf("%w: approval ended with status %d", ErrSSODenied, status)
	}

	interval := time.Duration(device.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	window := ssoPollWindow
	if expires := time.Duration(device.ExpiresIn) * time.Second; expires > 0 && expires < window {
		window = expires
	}
	tokenData, err := flow.pollToken(ctx, device.DeviceCode, interval, window)
	if err != nil {
		return nil, "", err
	}
	return tokenData, ssoTokenURL, nil
}

type ssoFlow struct {
	client *http.Client
	jar    http.CookieJar
}

func (f *ssoFlow) pollToken(ctx context.Context, deviceCode string, interval, window time.Duration) (*TokenData, error) {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if err := ssoSleep(ctx, interval); err != nil {
			return nil, err
		}
		status, _, body, err := f.do(ctx, http.MethodPost, ssoTokenURL, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {ClientID},
			"device_code": {deviceCode},
		})
		if err != nil {
			return nil, err
		}
		var payload struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			TokenType    string `json:"token_type"`
			ExpiresIn    int    `json:"expires_in"`
			Error        string `json:"error"`
		}
		if err = json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("parse the token response: %w", err)
		}
		if status >= 200 && status < 300 && strings.TrimSpace(payload.AccessToken) != "" {
			email, subject := parseJWTIdentity(payload.IDToken)
			return &TokenData{
				AccessToken:  strings.TrimSpace(payload.AccessToken),
				RefreshToken: strings.TrimSpace(payload.RefreshToken),
				IDToken:      strings.TrimSpace(payload.IDToken),
				TokenType:    strings.TrimSpace(payload.TokenType),
				ExpiresIn:    payload.ExpiresIn,
				Expire:       time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
				Email:        email,
				Subject:      subject,
			}, nil
		}
		switch payload.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied", "expired_token":
			return nil, ErrSSODenied
		}
		return nil, fmt.Errorf("token polling failed: status %d %s", status, payload.Error)
	}
	return nil, errors.New("token polling timed out")
}

// do sends one request and follows redirects within x.ai, returning the final
// status, the final URL and its body.
func (f *ssoFlow) do(ctx context.Context, method, endpoint string, form url.Values) (int, string, []byte, error) {
	current, err := url.Parse(endpoint)
	if err != nil || !ssoHostAllowed(current) {
		return 0, endpoint, nil, errors.New("refusing to send the grok session outside x.ai")
	}
	for hop := 0; hop <= ssoMaxRedirects; hop++ {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, errReq := http.NewRequestWithContext(ctx, method, current.String(), body)
		if errReq != nil {
			return 0, current.String(), nil, errReq
		}
		req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
		req.Header.Set("User-Agent", ssoUserAgent)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie := f.cookieHeader(current); cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, errDo := f.client.Do(req)
		if errDo != nil {
			return 0, current.String(), nil, fmt.Errorf("xai request failed: %w", errDo)
		}
		f.jar.SetCookies(current, resp.Cookies())
		data, errRead := io.ReadAll(io.LimitReader(resp.Body, ssoMaxBody))
		_ = resp.Body.Close()
		if errRead != nil {
			return resp.StatusCode, current.String(), nil, errRead
		}
		if resp.StatusCode < 300 || resp.StatusCode > 399 {
			return resp.StatusCode, current.String(), data, nil
		}
		location, errLoc := current.Parse(strings.TrimSpace(resp.Header.Get("Location")))
		if errLoc != nil || resp.Header.Get("Location") == "" {
			return resp.StatusCode, current.String(), data, errors.New("xai redirect without a usable Location")
		}
		if !ssoHostAllowed(location) {
			return resp.StatusCode, location.String(), data, errors.New("xai redirected outside x.ai")
		}
		if resp.StatusCode == http.StatusSeeOther ||
			((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) && method != http.MethodGet) {
			method, form = http.MethodGet, nil
		}
		current = location
	}
	return 0, current.String(), nil, errors.New("xai redirected too many times")
}

func (f *ssoFlow) cookieHeader(target *url.URL) string {
	cookies := f.jar.Cookies(target)
	sort.Slice(cookies, func(i, j int) bool { return cookies[i].Name < cookies[j].Name })
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}
