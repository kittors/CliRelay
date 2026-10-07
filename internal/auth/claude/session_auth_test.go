package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// fakeClaudeWeb answers the three calls a session login makes, all in memory.
type fakeClaudeWeb struct {
	t             *testing.T
	orgs          string
	orgsStatus    int
	orgsHeader    http.Header
	authorizeBody func(state string) string
	authorizeSeen map[string]string
	tokenSeen     map[string]any
}

func (f *fakeClaudeWeb) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(f.roundTrip)}
}

func (f *fakeClaudeWeb) roundTrip(req *http.Request) (*http.Response, error) {
	respond := func(status int, header http.Header, body string) (*http.Response, error) {
		if header == nil {
			header = make(http.Header)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	switch {
	case req.Method == http.MethodGet && req.URL.String() == "https://claude.ai/api/organizations":
		if got := req.Header.Get("Cookie"); got != "sessionKey=sk-ant-sid01-test" {
			f.t.Fatalf("organizations cookie = %q", got)
		}
		status := f.orgsStatus
		if status == 0 {
			status = http.StatusOK
		}
		return respond(status, f.orgsHeader, f.orgs)
	case req.Method == http.MethodPost && strings.HasPrefix(req.URL.String(), "https://claude.ai/v1/oauth/"):
		raw, _ := io.ReadAll(req.Body)
		f.authorizeSeen = map[string]string{"path": req.URL.Path}
		var body map[string]string
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Fatalf("authorize body: %v", err)
		}
		for key, value := range body {
			f.authorizeSeen[key] = value
		}
		return respond(http.StatusOK, nil, f.authorizeBody(body["state"]))
	case req.Method == http.MethodPost && req.URL.String() == TokenURL:
		raw, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(raw, &f.tokenSeen); err != nil {
			f.t.Fatalf("token body: %v", err)
		}
		return respond(http.StatusOK, nil, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,
			"account":{"uuid":"acct-1","email_address":"team.member@example.com"}}`)
	}
	f.t.Fatalf("unexpected request %s %s", req.Method, req.URL)
	return nil, nil
}

func redirectWithCode(state string) string {
	return `{"redirect_uri":"https://platform.claude.com/oauth/code/callback?code=auth-code-1&state=` + url.QueryEscape(state) + `"}`
}

func TestAuthorizeWithSessionKeyExchangesTheSessionForTokens(t *testing.T) {
	web := &fakeClaudeWeb{
		t: t,
		orgs: `[
			{"uuid":"org-api","name":"Console","capabilities":["api"]},
			{"uuid":"org-personal","name":"Personal","raven_type":null,"capabilities":["chat","claude_pro"]},
			{"uuid":"org-team","name":"Acme Team","raven_type":"team","capabilities":["chat","raven"]}
		]`,
		authorizeBody: redirectWithCode,
	}
	auth := &ClaudeAuth{httpClient: web.client()}

	bundle, org, err := auth.AuthorizeWithSessionKey(context.Background(), "sessionKey=sk-ant-sid01-test; other=1")
	if err != nil {
		t.Fatalf("AuthorizeWithSessionKey: %v", err)
	}
	if org != (SessionOrganization{UUID: "org-team", Name: "Acme Team"}) {
		t.Fatalf("organization = %+v, want the team organization", org)
	}
	if bundle.TokenData.Email != "team.member@example.com" || bundle.TokenData.RefreshToken != "rt-1" {
		t.Fatalf("bundle = %+v", bundle.TokenData)
	}

	seen := web.authorizeSeen
	if seen["path"] != "/v1/oauth/org-team/authorize" || seen["organization_uuid"] != "org-team" {
		t.Fatalf("authorize went to %q for %q", seen["path"], seen["organization_uuid"])
	}
	if seen["client_id"] != ClientID || seen["redirect_uri"] != PlatformRedirectURI || seen["scope"] != sessionScope {
		t.Fatalf("authorize body = %+v", seen)
	}
	if seen["code_challenge"] == "" || seen["code_challenge_method"] != "S256" || seen["state"] == "" {
		t.Fatalf("authorize body lacks PKCE/state: %+v", seen)
	}
	if web.tokenSeen["code"] != "auth-code-1" || web.tokenSeen["redirect_uri"] != PlatformRedirectURI {
		t.Fatalf("token exchange body = %+v", web.tokenSeen)
	}
	if web.tokenSeen["code_verifier"] == "" || web.tokenSeen["state"] != seen["state"] {
		t.Fatalf("token exchange must reuse the login's verifier and state: %+v", web.tokenSeen)
	}
}

func TestAuthorizeWithSessionKeySortsFailures(t *testing.T) {
	cases := []struct {
		name    string
		web     fakeClaudeWeb
		input   string
		wantErr error
	}{
		{
			name:    "refused session key",
			web:     fakeClaudeWeb{orgsStatus: http.StatusForbidden, orgs: `{"type":"error","error":{"type":"permission_error","message":"Invalid authorization"}}`},
			input:   "sk-ant-sid01-test",
			wantErr: ErrSessionKeyInvalid,
		},
		{
			name:    "cloudflare challenge",
			web:     fakeClaudeWeb{orgsStatus: http.StatusForbidden, orgs: `<!DOCTYPE html><title>Just a moment...</title>`},
			input:   "sk-ant-sid01-test",
			wantErr: ErrSessionBlocked,
		},
		{
			name:    "no organization",
			web:     fakeClaudeWeb{orgs: `[{"uuid":"org-api","name":"Console","capabilities":["api"]}]`},
			input:   "sk-ant-sid01-test",
			wantErr: ErrNoOrganization,
		},
		{
			name:    "not a session key",
			input:   "foo=bar; baz=qux",
			wantErr: ErrSessionKeyInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			web := tc.web
			web.t = t
			web.authorizeBody = redirectWithCode
			auth := &ClaudeAuth{httpClient: web.client()}
			_, _, err := auth.AuthorizeWithSessionKey(context.Background(), tc.input)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestAuthorizeWithSessionKeyRejectsAForeignState(t *testing.T) {
	web := &fakeClaudeWeb{
		t:             t,
		orgs:          `[{"uuid":"org-personal","name":"Personal"}]`,
		authorizeBody: func(string) string { return redirectWithCode("someone-else") },
	}
	auth := &ClaudeAuth{httpClient: web.client()}
	if _, _, err := auth.AuthorizeWithSessionKey(context.Background(), "sk-ant-sid01-test"); err == nil ||
		!strings.Contains(err.Error(), "different state") {
		t.Fatalf("error = %v, want a state mismatch", err)
	}
	if web.tokenSeen != nil {
		t.Fatal("a code issued for another state must not be exchanged")
	}
}

func TestNormalizeSessionKey(t *testing.T) {
	cases := map[string]string{
		"sk-ant-sid01-abc":                                "sk-ant-sid01-abc",
		`  "sk-ant-sid01-abc"  `:                          "sk-ant-sid01-abc",
		"sessionKey=sk-ant-sid01-abc; lastActiveOrg=org1": "sk-ant-sid01-abc",
		"Cookie: anthropic-device-id=x; sessionKey=sk-1":  "sk-1",
		"lastActiveOrg=org1; intercom-id=2":               "",
		"two words":                                       "",
		"":                                                "",
	}
	for input, want := range cases {
		if got := NormalizeSessionKey(input); got != want {
			t.Errorf("NormalizeSessionKey(%q) = %q, want %q", input, got, want)
		}
	}
}
