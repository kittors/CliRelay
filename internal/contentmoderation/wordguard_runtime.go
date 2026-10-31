package contentmoderation

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// WordPolicyResolver loads the word policies that govern one request. *Store
// implements it; the moderator picks it up from the profile resolver it is
// built with, so no wiring changes where the moderator is created. The policies
// it returns are shared and must not be modified: the moderator keeps what it
// built from a policy for as long as its version and save time stay the same.
type WordPolicyResolver interface {
	LoadWordPolicies(ctx context.Context, tenantID, keyID string) (WordPolicies, error)
}

var _ coreauth.RequestGuard = (*RuntimeModerator)(nil)

// How often the word guard writes the same kind of line to the process log. The
// request log keeps every hit; these lines only tell an operator it is
// happening, so a client that repeats one listed word does not turn into a line
// per request.
const (
	// policyErrorLogInterval spaces the warnings about an unreadable policy store,
	// which would otherwise be one per request for as long as the outage lasts.
	policyErrorLogInterval = 30 * time.Second
	// wordHitLogInterval is the spacing of the lines about one tenant's hits on
	// one rule.
	wordHitLogInterval = 10 * time.Second
	// regexSkippedLogInterval is the spacing of the lines about requests too large
	// for the regular expressions' work budget, per tenant.
	regexSkippedLogInterval = time.Minute
)

// wordGuardKeyID returns the API key the request was authenticated with. The
// id (not the key) is what a key policy hangs on.
func wordGuardKeyID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if ginCtx == nil {
		return ""
	}
	value, exists := ginCtx.Get("accessMetadata")
	if !exists {
		return ""
	}
	metadata, _ := value.(map[string]string)
	return strings.TrimSpace(metadata["api-key-id"])
}

// GuardRequest implements coreauth.RequestGuard. It checks the request against
// the platform, tenant and API key word policies before any account is chosen,
// and either refuses it, hands back a copy with the hits replaced, or lets it
// through (noting hits in the request log).
//
// It fails open: policies that cannot be read leave the request as it is,
// unless some were read before, in which case those still apply.
func (m *RuntimeModerator) GuardRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) coreauth.RequestGuardResult {
	if m == nil || m.words == nil {
		return coreauth.RequestGuardResult{}
	}
	tenantID := coreauth.NormalizedTenantID(metadataString(opts.Metadata, cliproxyexecutor.TenantMetadataKey))
	policies, err := m.words.LoadWordPolicies(ctx, tenantID, wordGuardKeyID(ctx))
	policyError := ""
	if err != nil {
		policyError = "policy_store_error"
		m.warnPolicyStore(tenantID, err)
	}
	contexts := m.wordContexts.contextsFor(policies.Platform, policies.Tenant, policies.Key)
	if len(contexts) == 0 {
		return coreauth.RequestGuardResult{}
	}

	format := opts.SourceFormat
	body := opts.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	// One budget for the whole request, however many bodies are checked.
	budget := newRegexBudget()
	verdict := evaluateWords(format, body, contexts, wordEvalOptions{budget: budget})
	// The executors translate req.Payload and read opts.OriginalRequest. They are
	// the same bytes unless a caller set them apart, in which case the payload is
	// checked on its own.
	separate := len(req.Payload) > 0 && len(opts.OriginalRequest) > 0 && !sameStorage(req.Payload, opts.OriginalRequest)
	var payloadVerdict WordVerdict
	if separate {
		if bytes.Equal(req.Payload, opts.OriginalRequest) {
			payloadVerdict = verdict
		} else {
			payloadVerdict = evaluateWords(format, req.Payload, contexts, wordEvalOptions{budget: budget})
		}
		if payloadVerdict.Blocked && !verdict.Blocked {
			verdict = payloadVerdict
		}
	}

	verdict.RegexSkipped = budget.wasExceeded()
	snapshot := newWordGuardSnapshot(verdict, contexts, policyError)
	SetWordGuardSnapshot(ctx, snapshot)
	m.logWordVerdict(tenantID, verdict, len(body))
	if verdict.Blocked {
		return coreauth.RequestGuardResult{Blocked: true, Message: verdict.BlockMessage, HTTPStatus: DefaultBlockStatus}
	}

	result := coreauth.RequestGuardResult{Reply: newWordReply(contexts, format, snapshot)}
	switch {
	case verdict.Payload != nil && separate:
		result.OriginalRequest = verdict.Payload
		result.Payload = req.Payload
		if payloadVerdict.Payload != nil {
			result.Payload = payloadVerdict.Payload
		}
	case verdict.Payload != nil && len(opts.OriginalRequest) == 0:
		result.Payload = verdict.Payload
	case verdict.Payload != nil:
		// One body: both names keep pointing at one slice, which is how the
		// executors know not to translate it twice.
		result.OriginalRequest, result.Payload = verdict.Payload, verdict.Payload
	case separate && payloadVerdict.Payload != nil:
		result.Payload = payloadVerdict.Payload
	}
	return result
}

func sameStorage(a, b []byte) bool {
	return len(a) > 0 && len(a) == len(b) && &a[0] == &b[0]
}

// warnPolicyStore logs an unreadable policy store, at most once in a while.
func (m *RuntimeModerator) warnPolicyStore(tenantID string, err error) {
	ok, dropped := m.logs.allow("policy_store", policyErrorLogInterval, time.Now())
	if !ok {
		return
	}
	log.WithFields(log.Fields{"tenant_id": tenantID, "suppressed": dropped}).WithError(err).Warn("word guard could not read its policies; using the ones it last saw")
}

// logWordVerdict logs what the guard did with a request. It names rules and
// layers, never the words or the text, and writes at most one line in a while
// for the same tenant and rule.
func (m *RuntimeModerator) logWordVerdict(tenantID string, verdict WordVerdict, bodyBytes int) {
	now := time.Now()
	if verdict.RegexSkipped {
		if ok, dropped := m.logs.allow("regex_skipped\x00"+tenantID, regexSkippedLogInterval, now); ok {
			log.WithFields(log.Fields{
				"tenant_id":  tenantID,
				"body_bytes": bodyBytes,
				"suppressed": dropped,
			}).Warn("word guard did not run some regular expressions: the request is larger than their work budget; keywords were still checked")
		}
	}
	if verdict.MatchCount == 0 && !verdict.Blocked {
		return
	}
	rule := ""
	if len(verdict.Matches) > 0 {
		rule = verdict.Matches[0].RuleID
	}
	key := "hit\x00" + tenantID + "\x00" + rule + "\x00" + verdict.Action + "\x00" + verdict.Reason
	ok, dropped := m.logs.allow(key, wordHitLogInterval, now)
	if !ok {
		return
	}
	fields := log.Fields{
		"tenant_id":   tenantID,
		"action":      verdict.Action,
		"matches":     verdict.MatchCount,
		"replaced":    verdict.Replaced,
		"latency_ms":  verdict.LatencyMS,
		"reason":      verdict.Reason,
		"first_rules": firstRuleIDs(verdict.Matches, 5),
	}
	if verdict.Field != "" {
		fields["field"] = verdict.Field
	}
	if dropped > 0 {
		fields["suppressed"] = dropped
	}
	if len(verdict.Matches) > 0 {
		fields["layer"] = verdict.Matches[0].Layer
	}
	entry := log.WithFields(fields)
	if verdict.Blocked {
		entry.Warn("word guard blocked request")
		return
	}
	entry.Info("word guard matched request")
}

func firstRuleIDs(matches []WordMatch, limit int) []string {
	ids := make([]string, 0, min(len(matches), limit))
	for _, match := range matches {
		if len(ids) == limit {
			break
		}
		ids = append(ids, match.RuleID)
	}
	return ids
}
