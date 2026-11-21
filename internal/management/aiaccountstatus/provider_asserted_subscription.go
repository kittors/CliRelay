package aiaccountstatus

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type providerAssertedAccountFacts struct {
	PlanType     string
	Subscription usage.AIAccountSubscription
}

// mergeProviderAssertedFacts fills in what the probe itself did not report: the
// plan type, and the subscription when no live billing read happened this
// round. A live read is never replaced; subscriptionUpsertAssignments keeps the
// same rule for the stored row.
func mergeProviderAssertedFacts(probe *ProbeResult, auth *coreauth.Auth, now time.Time) {
	asserted := readProviderAssertedAccountFacts(auth, now)
	if strings.TrimSpace(probe.PlanType) == "" {
		probe.PlanType = asserted.PlanType
	}
	if probe.Subscription.Source == "" {
		probe.Subscription = asserted.Subscription
	}
}

// readProviderAssertedAccountFacts only consumes provider token claims. Auth-file
// subscription fields are tenant-private manual overrides and must never enter
// the shared subject status table. Stored OAuth tokens were obtained through the
// provider authentication flow; this helper only decodes their already-issued
// claims and never persists token material.
//
// The subscription claims are a snapshot taken at the OAuth login: Codex
// re-issues the id_token on refresh without updating them (live 2026-11-20:
// tokens issued 11-19 still said active_until 10-10, last_checked 09-10). The
// snapshot is used only while its end is still ahead; past it, the claims say
// nothing about whether the plan renewed.
func readProviderAssertedAccountFacts(auth *coreauth.Auth, now time.Time) providerAssertedAccountFacts {
	if auth == nil || normalizeProvider(auth.Provider) != "codex" {
		return providerAssertedAccountFacts{}
	}
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil || !identity.ShareEligible || strings.TrimSpace(identity.AccountID) == "" {
		return providerAssertedAccountFacts{}
	}
	for _, token := range []string{
		metadataString(auth, "id_token", "idToken"),
		metadataString(auth, "access_token", "accessToken"),
	} {
		claims, ok := decodeProviderJWTClaims(token)
		if !ok {
			continue
		}
		authClaims, _ := claims["https://api.openai.com/auth"].(map[string]any)
		if len(authClaims) == 0 {
			continue
		}
		accountID := strings.TrimSpace(readClaimString(authClaims, "chatgpt_account_id", "account_id"))
		if accountID == "" || accountID != strings.TrimSpace(identity.AccountID) {
			continue
		}
		facts := providerAssertedAccountFacts{
			PlanType: strings.ToLower(strings.TrimSpace(readClaimString(authClaims, "chatgpt_plan_type", "plan_type"))),
		}
		if expires := parseProviderClaimTime(authClaims["chatgpt_subscription_active_until"]); expires != nil && expires.After(now) {
			facts.Subscription = usage.AIAccountSubscription{
				StartedAt: parseProviderClaimTime(authClaims["chatgpt_subscription_active_start"]),
				ExpiresAt: expires, Source: usage.SubscriptionSourceSignedClaims,
			}
		}
		return facts
	}
	return providerAssertedAccountFacts{}
}

func decodeProviderJWTClaims(token string) (map[string]any, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

func readClaimString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseProviderClaimTime(value any) *time.Time {
	var parsed time.Time
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil
		}
		if unix, err := strconv.ParseInt(text, 10, 64); err == nil && unix > 0 {
			parsed = time.Unix(unix, 0).UTC()
		} else if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
			parsed = t.UTC()
		} else if t, err := time.Parse(time.RFC3339, text); err == nil {
			parsed = t.UTC()
		}
	case float64:
		if v > 0 {
			parsed = time.Unix(int64(v), 0).UTC()
		}
	case json.Number:
		if unix, err := v.Int64(); err == nil && unix > 0 {
			parsed = time.Unix(unix, 0).UTC()
		}
	case int64:
		if v > 0 {
			parsed = time.Unix(v, 0).UTC()
		}
	case int:
		if v > 0 {
			parsed = time.Unix(int64(v), 0).UTC()
		}
	}
	if parsed.IsZero() {
		return nil
	}
	return &parsed
}
