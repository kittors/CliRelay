package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// Authorizing with a claude.ai browser session.
//
// An operator who is signed in to claude.ai holds a sessionKey cookie. With it
// the account's organization can be read and Claude Code's OAuth client
// authorized on the account's behalf — the same consent the browser login asks
// for, answered by the session instead of a click. The resulting code is then
// exchanged exactly like the browser flow's, so the stored account is
// indistinguishable from one added by signing in.
//
// claude.ai sits behind Cloudflare. ClaudeAuth's client already carries the
// utls fingerprint that keeps Anthropic domains from challenging it; a
// challenge that still comes back is reported as ErrSessionBlocked, since it
// says nothing about the session key itself.

// claudeWebBaseURL is claude.ai; tests point it at a fake.
var claudeWebBaseURL = "https://claude.ai"

// sessionScope is what the authorize API grants a session. The browser flow
// also asks for org:create_api_key, which this API does not accept.
const sessionScope = "user:profile user:inference"

// sessionUserAgent matches the Firefox ClientHello the transport sends; a
// Chrome user agent on a Firefox handshake would itself stand out.
const sessionUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:133.0) Gecko/20100101 Firefox/133.0"

var (
	// ErrSessionKeyInvalid: claude.ai does not recognise the session key.
	ErrSessionKeyInvalid = errors.New("claude.ai session key is invalid or expired")
	// ErrSessionBlocked: claude.ai answered with an anti-bot challenge.
	ErrSessionBlocked = errors.New("claude.ai blocked the request")
	// ErrNoOrganization: the session's account has no organization to authorize.
	ErrNoOrganization = errors.New("claude.ai account has no organization")
)

// SessionOrganization is the organization a session login authorized.
type SessionOrganization struct {
	UUID string
	Name string
}

// NormalizeSessionKey extracts a session key from what operators paste: the
// bare value, or a cookie header that contains sessionKey=…. It returns "" for
// anything that cannot be a session key.
func NormalizeSessionKey(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}
	if strings.Contains(strings.ToLower(value), "sessionkey=") {
		for _, part := range strings.Split(value, ";") {
			name, key, found := strings.Cut(strings.TrimSpace(part), "=")
			if found && strings.EqualFold(strings.TrimSpace(name), "sessionKey") {
				return strings.Trim(strings.TrimSpace(key), `"'`)
			}
		}
		return ""
	}
	if value == "" || strings.ContainsAny(value, " \t\r\n;,") {
		return ""
	}
	return value
}

// AuthorizeWithSessionKey turns a claude.ai session into OAuth tokens and
// reports which organization was authorized.
func (o *ClaudeAuth) AuthorizeWithSessionKey(ctx context.Context, sessionKey string) (*ClaudeAuthBundle, SessionOrganization, error) {
	key := NormalizeSessionKey(sessionKey)
	if key == "" {
		return nil, SessionOrganization{}, ErrSessionKeyInvalid
	}
	org, err := o.sessionOrganization(ctx, key)
	if err != nil {
		return nil, SessionOrganization{}, err
	}
	pkceCodes, err := GeneratePKCECodes()
	if err != nil {
		return nil, org, fmt.Errorf("generate PKCE codes: %w", err)
	}
	state, err := generateCodeVerifier()
	if err != nil {
		return nil, org, fmt.Errorf("generate state: %w", err)
	}
	code, err := o.sessionAuthorizationCode(ctx, key, org.UUID, pkceCodes, state)
	if err != nil {
		return nil, org, err
	}
	bundle, err := o.ExchangeCodeForTokensWithRedirectURI(ctx, code, state, pkceCodes, PlatformRedirectURI)
	if err != nil {
		return nil, org, err
	}
	return bundle, org, nil
}

type sessionOrganizationEntry struct {
	UUID         string   `json:"uuid"`
	Name         string   `json:"name"`
	RavenType    *string  `json:"raven_type"`
	Capabilities []string `json:"capabilities"`
}

func (o *ClaudeAuth) sessionOrganization(ctx context.Context, sessionKey string) (SessionOrganization, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeWebBaseURL+"/api/organizations", nil)
	if err != nil {
		return SessionOrganization{}, fmt.Errorf("create organizations request: %w", err)
	}
	setSessionHeaders(req, sessionKey)
	body, err := o.doSessionRequest(req)
	if err != nil {
		return SessionOrganization{}, err
	}
	var orgs []sessionOrganizationEntry
	if err = json.Unmarshal(body, &orgs); err != nil {
		return SessionOrganization{}, fmt.Errorf("parse organizations: %w", err)
	}
	return pickSessionOrganization(orgs)
}

// pickSessionOrganization chooses which organization to authorize. Console
// (API-only) organizations cannot use a subscription, so organizations that
// can chat come first; among those a team organization wins, since that is
// where a Team seat's usage lives — the same preference sub2api settled on.
func pickSessionOrganization(orgs []sessionOrganizationEntry) (SessionOrganization, error) {
	candidates := make([]sessionOrganizationEntry, 0, len(orgs))
	for _, org := range orgs {
		if strings.TrimSpace(org.UUID) == "" {
			continue
		}
		if len(org.Capabilities) > 0 && !containsFold(org.Capabilities, "chat") {
			continue
		}
		candidates = append(candidates, org)
	}
	if len(candidates) == 0 {
		return SessionOrganization{}, ErrNoOrganization
	}
	chosen := candidates[0]
	for _, org := range candidates {
		if org.RavenType != nil && strings.EqualFold(*org.RavenType, "team") {
			chosen = org
			break
		}
	}
	return SessionOrganization{UUID: strings.TrimSpace(chosen.UUID), Name: strings.TrimSpace(chosen.Name)}, nil
}

func (o *ClaudeAuth) sessionAuthorizationCode(ctx context.Context, sessionKey, orgUUID string, pkceCodes *PKCECodes, state string) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"response_type":         "code",
		"client_id":             ClientID,
		"organization_uuid":     orgUUID,
		"redirect_uri":          PlatformRedirectURI,
		"scope":                 sessionScope,
		"state":                 state,
		"code_challenge":        pkceCodes.CodeChallenge,
		"code_challenge_method": "S256",
	})
	if err != nil {
		return "", fmt.Errorf("encode authorize request: %w", err)
	}
	endpoint := claudeWebBaseURL + "/v1/oauth/" + url.PathEscape(orgUUID) + "/authorize"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create authorize request: %w", err)
	}
	setSessionHeaders(req, sessionKey)
	req.Header.Set("Content-Type", "application/json")
	body, err := o.doSessionRequest(req)
	if err != nil {
		return "", err
	}
	var result struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse authorize response: %w", err)
	}
	redirect, err := url.Parse(strings.TrimSpace(result.RedirectURI))
	if err != nil || result.RedirectURI == "" {
		return "", errors.New("authorize response has no redirect_uri")
	}
	query := redirect.Query()
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		return "", errors.New("authorize response has no authorization code")
	}
	if returned := strings.TrimSpace(query.Get("state")); returned != "" && returned != state {
		return "", errors.New("authorize response carries a different state")
	}
	return code, nil
}

func setSessionHeaders(req *http.Request, sessionKey string) {
	req.Header.Set("Cookie", "sessionKey="+sessionKey)
	req.Header.Set("User-Agent", sessionUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", claudeWebBaseURL)
	req.Header.Set("Referer", claudeWebBaseURL+"/new")
}

// doSessionRequest sends a claude.ai request and sorts its failures: a
// challenge page is ErrSessionBlocked, 401/403 otherwise means the session key
// was refused.
func (o *ClaudeAuth) doSessionRequest(req *http.Request) ([]byte, error) {
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude.ai request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := util.ReadHTTPResponseBody("claude-session", resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read claude.ai response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	if util.IsAntiBotChallenge(resp.Header, body) {
		return nil, fmt.Errorf("%w (status %d)", ErrSessionBlocked, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w (status %d)", ErrSessionKeyInvalid, resp.StatusCode)
	}
	return nil, fmt.Errorf("claude.ai responded with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}
