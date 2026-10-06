package xai

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type ssoRoundTrip func(*http.Request) (*http.Response, error)

func (f ssoRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func ssoResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func instantSSOSleep(t *testing.T) {
	t.Helper()
	previous := ssoSleep
	ssoSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { ssoSleep = previous })
}

func fakeIDToken(email string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `","sub":"user-sub-1"}`))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig"
}

// fakeXAI plays accounts.x.ai and auth.x.ai entirely in memory.
type fakeXAI struct {
	t            *testing.T
	signedOut    bool
	tokenReplies []string
	approveForm  url.Values
	cookiesSeen  map[string]string
}

func (f *fakeXAI) handle(req *http.Request) (*http.Response, error) {
	f.cookiesSeen[req.Method+" "+req.URL.Path] = req.Header.Get("Cookie")
	form := url.Values{}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		form, _ = url.ParseQuery(string(raw))
	}
	switch {
	case req.Method == http.MethodGet && req.URL.String() == ssoAccountsURL:
		if f.signedOut {
			return ssoResponse(req, http.StatusFound, http.Header{"Location": {"https://accounts.x.ai/sign-in?redirect=%2F"}}, ""), nil
		}
		return ssoResponse(req, http.StatusOK, nil, "<html>account</html>"), nil
	case req.Method == http.MethodGet && req.URL.Host == "accounts.x.ai" && req.URL.Path == "/sign-in":
		return ssoResponse(req, http.StatusOK, nil, "<html>sign in</html>"), nil
	case req.Method == http.MethodPost && req.URL.String() == ssoDeviceCodeURL:
		if form.Get("client_id") != ClientID || form.Get("scope") != Scope {
			f.t.Fatalf("device code form = %v", form)
		}
		header := http.Header{"Set-Cookie": {"csrf=csrf-token-1; Path=/; Secure"}}
		return ssoResponse(req, http.StatusOK, header, `{"device_code":"dev-1","user_code":"ABCD-1234",
			"verification_uri_complete":"https://auth.x.ai/oauth2/device/verify?user_code=ABCD-1234","interval":1,"expires_in":600}`), nil
	case req.Method == http.MethodGet && req.URL.Path == "/oauth2/device/verify":
		return ssoResponse(req, http.StatusOK, nil, "<html>enter code</html>"), nil
	case req.Method == http.MethodPost && req.URL.String() == ssoVerifyURL:
		if form.Get("user_code") != "ABCD-1234" {
			f.t.Fatalf("verify form = %v", form)
		}
		return ssoResponse(req, http.StatusFound, http.Header{"Location": {"/oauth2/device/consent?user_code=ABCD-1234"}}, ""), nil
	case req.Method == http.MethodGet && req.URL.Path == "/oauth2/device/consent":
		return ssoResponse(req, http.StatusOK, nil, "<html>consent</html>"), nil
	case req.Method == http.MethodPost && req.URL.String() == ssoApproveURL:
		f.approveForm = form
		return ssoResponse(req, http.StatusSeeOther, http.Header{"Location": {"/oauth2/device/done"}}, ""), nil
	case req.Method == http.MethodGet && req.URL.Path == "/oauth2/device/done":
		return ssoResponse(req, http.StatusOK, nil, "<html>done</html>"), nil
	case req.Method == http.MethodPost && req.URL.String() == ssoTokenURL:
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || form.Get("device_code") != "dev-1" {
			f.t.Fatalf("token form = %v", form)
		}
		reply := f.tokenReplies[0]
		f.tokenReplies = f.tokenReplies[1:]
		if strings.Contains(reply, `"error"`) {
			return ssoResponse(req, http.StatusBadRequest, nil, reply), nil
		}
		return ssoResponse(req, http.StatusOK, nil, reply), nil
	}
	f.t.Fatalf("unexpected request %s %s", req.Method, req.URL)
	return nil, nil
}

func newFakeXAI(t *testing.T) (*fakeXAI, *XAIAuth) {
	fake := &fakeXAI{t: t, cookiesSeen: map[string]string{}, tokenReplies: []string{
		`{"error":"authorization_pending"}`,
		`{"access_token":"xai-at-1","refresh_token":"xai-rt-1","id_token":"` + fakeIDToken("linus@example.net") + `","token_type":"Bearer","expires_in":21600}`,
	}}
	return fake, &XAIAuth{httpClient: &http.Client{Transport: ssoRoundTrip(fake.handle)}}
}

func TestConvertSSOToTokensApprovesTheDeviceWithTheSession(t *testing.T) {
	instantSSOSleep(t)
	fake, auth := newFakeXAI(t)

	tokens, endpoint, err := auth.ConvertSSOToTokens(context.Background(), "sso=fake-sso-token; sso-rw=fake-sso-token; _ga=1")
	if err != nil {
		t.Fatalf("ConvertSSOToTokens: %v", err)
	}
	if tokens.AccessToken != "xai-at-1" || tokens.RefreshToken != "xai-rt-1" || tokens.Email != "linus@example.net" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if endpoint != ssoTokenURL {
		t.Fatalf("endpoint = %q", endpoint)
	}
	if fake.approveForm.Get("action") != "allow" || fake.approveForm.Get("user_code") != "ABCD-1234" {
		t.Fatalf("approve form = %v", fake.approveForm)
	}
	accounts := fake.cookiesSeen["GET /"]
	if !strings.Contains(accounts, "sso=fake-sso-token") || !strings.Contains(accounts, "sso-rw=fake-sso-token") {
		t.Fatalf("accounts.x.ai cookies = %q", accounts)
	}
	// The csrf cookie auth.x.ai set on the device-code response must ride along
	// with the confirmation and the approval.
	for _, step := range []string{"POST /oauth2/device/verify", "POST /oauth2/device/approve"} {
		if cookies := fake.cookiesSeen[step]; !strings.Contains(cookies, "csrf=csrf-token-1") || !strings.Contains(cookies, "sso=fake-sso-token") {
			t.Fatalf("%s cookies = %q", step, cookies)
		}
	}
}

func TestConvertSSOToTokensReportsASignedOutSession(t *testing.T) {
	instantSSOSleep(t)
	fake, auth := newFakeXAI(t)
	fake.signedOut = true
	if _, _, err := auth.ConvertSSOToTokens(context.Background(), "fake-sso-token"); !errors.Is(err, ErrSSOInvalid) {
		t.Fatalf("error = %v, want ErrSSOInvalid", err)
	}
}

func TestConvertSSOToTokensReportsADeniedAuthorization(t *testing.T) {
	instantSSOSleep(t)
	fake, auth := newFakeXAI(t)
	fake.tokenReplies = []string{`{"error":"access_denied"}`}
	if _, _, err := auth.ConvertSSOToTokens(context.Background(), "fake-sso-token"); !errors.Is(err, ErrSSODenied) {
		t.Fatalf("error = %v, want ErrSSODenied", err)
	}
}

func TestConvertSSOToTokensNeverFollowsTheSessionOffXAI(t *testing.T) {
	instantSSOSleep(t)
	var leaked bool
	auth := &XAIAuth{httpClient: &http.Client{Transport: ssoRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "evil.example.com" {
			leaked = true
		}
		return ssoResponse(req, http.StatusFound, http.Header{"Location": {"https://evil.example.com/steal"}}, ""), nil
	})}}
	_, _, err := auth.ConvertSSOToTokens(context.Background(), "fake-sso-token")
	if err == nil || !strings.Contains(err.Error(), "outside x.ai") {
		t.Fatalf("error = %v, want a refusal to leave x.ai", err)
	}
	if leaked {
		t.Fatal("the session cookie was sent off x.ai")
	}
}

func TestNormalizeSSOToken(t *testing.T) {
	cases := map[string]string{
		"fake-sso-token":                         "fake-sso-token",
		`"fake-sso-token"`:                       "fake-sso-token",
		"sso=abc; sso-rw=abc; _ga=GA1":           "abc",
		"Cookie: _ga=GA1; sso-rw=xyz":            "xyz",
		"sso=only":                               "only",
		"_ga=GA1; cf_clearance=x":                "",
		"two words":                              "",
		strings.Repeat("a", ssoMaxTokenLength+1): "",
	}
	for input, want := range cases {
		if got := NormalizeSSOToken(input); got != want {
			t.Errorf("NormalizeSSOToken(%.40q) = %q, want %q", input, got, want)
		}
	}
}
