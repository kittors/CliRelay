package antigravity

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRefreshAccessTokenUsesTheConfiguredClient(t *testing.T) {
	var form url.Values
	client := &http.Client{Transport: discoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != TokenEndpoint {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		raw, _ := io.ReadAll(req.Body)
		form, _ = url.ParseQuery(string(raw))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"ya29.fake","expires_in":3599,"token_type":"Bearer"}`)),
		}, nil
	})}
	auth := NewAntigravityAuth(nil, client)

	token, err := auth.RefreshAccessToken(context.Background(), " 1//fake-refresh ")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if token.AccessToken != "ya29.fake" || token.ExpiresIn != 3599 {
		t.Fatalf("token = %+v", token)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "1//fake-refresh" {
		t.Fatalf("form = %v", form)
	}
	if form.Get("client_id") == "" || form.Get("client_id") != auth.clientID {
		t.Fatalf("refresh must use the configured antigravity client, got %q", form.Get("client_id"))
	}
}

func TestRefreshAccessTokenReportsTheUpstreamRefusal(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)),
		}, nil
	})}
	_, err := NewAntigravityAuth(nil, client).RefreshAccessToken(context.Background(), "1//fake-refresh")
	if err == nil || !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error = %v, want the status and the upstream error", err)
	}
}
