package alerting

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The egress rule reports a proxy the tenant's traffic leaves through that the
// egress breaker has shut, and reports again once requests get through it.
//
// What a proxy's breaker knows lives in the memory of the process that sent
// the requests (see package egressbreaker), and the process evaluating the
// rule only knows what it saw itself. That shapes the rule:
//
//   - A process that started after an outage began (a deploy, a restart) has
//     not seen the proxy fail yet, so "not open" proves nothing. An outage is
//     over only once a request got through the proxy after it began.
//   - During a deploy the old backend slot keeps evaluating for a few minutes
//     with the breaker as it was when its requests stopped. Its stale outage
//     must not be raised again right after the new slot saw the end of it,
//     which is what SentRecord.Resolved and resolvedQuiet are for.
//   - In a cluster only the leader evaluates and it sees its own breaker. A
//     provider refusing the credentials fails every node alike; a route that
//     fails on one node only is what the DNS watcher takes the node out for.

// Kinds of proxy a tenant's traffic leaves through (EgressSample.Kind), by
// where the proxy is configured.
const (
	// EgressPoolEntry is an entry of the tenant's own proxy pool.
	EgressPoolEntry = "pool"
	// EgressSharedEntry is a proxy the platform shares with the tenant.
	EgressSharedEntry = "shared"
	// EgressAccountProxy is a proxy URL set on the accounts themselves.
	EgressAccountProxy = "account"
	// EgressDefaultProxy is the proxy-url of the configuration, which every
	// account without a proxy of its own uses.
	EgressDefaultProxy = "default"
)

// Faults of a proxy (EgressSample.Fault), as the egress breaker names them.
const (
	EgressFaultAuthRejected = "auth_rejected"
	EgressFaultUnreachable  = "unreachable"
)

// EgressSample is one proxy the tenant's traffic leaves through, as the egress
// breaker of the evaluating process sees it.
type EgressSample struct {
	Kind string
	// ID and Name are the proxy-pool entry's, empty for a proxy that is not one.
	ID   string
	Name string
	// Endpoint is the proxy's scheme, host and port. Never its credentials.
	Endpoint string
	// Fingerprint stands for the whole address, credentials included, without
	// revealing it. An entry whose address changed is a different proxy.
	Fingerprint string
	// Accounts counts the tenant's switched-on accounts that leave through it.
	Accounts int

	// Open says the breaker refuses requests through the proxy; the fields up
	// to FailedProbes describe the outage.
	Open  bool
	Fault string
	// Since is when the breaker opened.
	Since time.Time
	// NextProbe is when a request is next let through to try the proxy. It is
	// in the past when no request has come since the wait ended.
	NextProbe time.Time
	// Refused counts the attempts turned away, one per account a request tried.
	Refused int64
	// FailedProbes counts the probes that found the proxy still failing.
	FailedProbes int
	// LastPass is when a request last got through the proxy in this process,
	// zero when none has lately.
	LastPass time.Time
}

// egressKeyPrefix starts the dedupe key of an outage. The key names the proxy
// by its pool entry when it is one, so that the alert follows the entry when
// its address changes, and by the fingerprint of the address:
// "egress_breaker|entry:sg-1|<fingerprint>", "egress_breaker|default:|<fingerprint>".
// Entry IDs hold letters, digits and dashes only, so neither separator can
// occur in one.
const egressKeyPrefix = RuleEgressBreaker + "|"

const egressEntryRef = "entry:"

func egressRef(s EgressSample) string {
	if s.ID != "" {
		return egressEntryRef + s.ID
	}
	return s.Kind + ":"
}

func egressKey(s EgressSample) string {
	return egressKeyPrefix + egressRef(s) + "|" + s.Fingerprint
}

// egressEndKey is the key of the message that the outage of key is over.
func egressEndKey(key string) string { return key + "|ok" }

// splitEgressKey returns the proxy and fingerprint of an outage key, and false
// for any other key, those of end messages included.
func splitEgressKey(key string) (ref, fingerprint string, ok bool) {
	rest, found := strings.CutPrefix(key, egressKeyPrefix)
	if !found {
		return "", "", false
	}
	ref, fingerprint, found = strings.Cut(rest, "|")
	if !found || strings.Contains(fingerprint, "|") {
		return "", "", false
	}
	return ref, fingerprint, true
}

// egressCandidates finds the proxies the breaker has kept shut for long enough
// to report. Besides the time, at least one probe must have failed after the
// breaker opened. Two rejections or three failed connects open it within
// moments, and the probe 15 seconds later is the first sign that this was not
// a blip; an outage no request came to probe is a guess that the next request
// settles.
func egressCandidates(rule EgressBreakerRule, samples []EgressSample, now time.Time) []Candidate {
	var out []Candidate
	for _, s := range samples {
		if !s.Open || s.FailedProbes < 1 || s.Since.IsZero() {
			continue
		}
		lasted := now.Sub(s.Since)
		if lasted < time.Duration(rule.ForMinutes)*time.Minute {
			continue
		}
		facts := egressFacts(s)
		facts["fault"] = s.Fault
		facts["since"] = s.Since.UTC().Format(time.RFC3339)
		facts["open_minutes"] = int(lasted / time.Minute)
		facts["for_minutes"] = rule.ForMinutes
		facts["accounts"] = s.Accounts
		facts["refused"] = s.Refused
		facts["failed_probes"] = s.FailedProbes
		if !s.NextProbe.IsZero() {
			facts["next_probe_at"] = s.NextProbe.UTC().Format(time.RFC3339)
		}
		out = append(out, Candidate{
			Rule:     RuleEgressBreaker,
			Severity: SeverityCritical,
			Key:      egressKey(s),
			Since:    s.Since,
			Subject:  egressSubject(s),
			Facts:    facts,
			Describe: func(language string) string { return describeOutage(s, lasted, now, language) },
		})
	}
	return out
}

// egressRound is what one round read about the tenant's proxies.
type egressRound struct {
	samples []EgressSample
}

// recoveries returns a message for each reported outage that is over, and
// forgets the outages of proxies the tenant no longer leaves through.
//
// An outage is over once the breaker is closed and a request got through the
// proxy after the outage began. Short of that, a reported outage stays as it
// is until its record runs out (one cooldown) and is then dropped without a
// word: a quiet record is better than a false "it works again".
func (r *egressRound) recoveries(sent *SentState, now time.Time) []Candidate {
	if r == nil || sent == nil {
		return nil
	}
	entries := make(map[string]EgressSample, len(r.samples))
	others := make(map[string]EgressSample, len(r.samples))
	for _, s := range r.samples {
		if s.ID != "" {
			entries[egressRef(s)] = s
		} else {
			others[egressRef(s)+"|"+s.Fingerprint] = s
		}
	}
	keys := make([]string, 0, len(sent.Records))
	for key := range sent.Records {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var out []Candidate
	for _, key := range keys {
		record := sent.Records[key]
		ref, fingerprint, ok := splitEgressKey(key)
		if !ok || !record.reported() {
			continue
		}
		var current EgressSample
		var found bool
		if strings.HasPrefix(ref, egressEntryRef) {
			current, found = entries[ref]
		} else {
			current, found = others[ref+"|"+fingerprint]
		}
		if !found {
			// The entry was removed or switched off, or no account leaves through
			// the proxy any more: somebody already dealt with it.
			delete(sent.Records, key)
			continue
		}
		moved := current.Fingerprint != fingerprint
		if current.Open {
			if moved {
				// The entry now points at an address that fails as well. That is a
				// new outage, reported under its own key.
				delete(sent.Records, key)
			}
			continue
		}
		if current.LastPass.IsZero() || !current.LastPass.After(record.Since) {
			continue
		}
		out = append(out, endOfOutage(key, record, current, moved))
	}
	return out
}

func endOfOutage(key string, record SentRecord, s EgressSample, moved bool) Candidate {
	facts := egressFacts(s)
	facts["recovered_at"] = s.LastPass.UTC().Format(time.RFC3339)
	facts["address_changed"] = moved
	var lasted time.Duration
	if !record.Since.IsZero() {
		lasted = s.LastPass.Sub(record.Since)
		facts["since"] = record.Since.UTC().Format(time.RFC3339)
		facts["outage_minutes"] = int(lasted / time.Minute)
	}
	return Candidate{
		Rule:     RuleEgressRecovered,
		Severity: SeverityInfo,
		Key:      egressEndKey(key),
		Resolves: key,
		Subject:  egressSubject(s),
		Facts:    facts,
		Describe: func(language string) string { return describeRecovery(s, lasted, moved, language) },
	}
}

func egressFacts(s EgressSample) map[string]any {
	facts := map[string]any{"proxy_kind": s.Kind, "endpoint": s.Endpoint}
	if s.ID != "" {
		facts["proxy_id"] = s.ID
	}
	return facts
}

// egressSubject names the proxy by its entry; a proxy that is not one by where
// it is configured.
func egressSubject(s EgressSample) Subject {
	name := cleanText(s.Name)
	switch {
	case name != "":
	case s.Kind == EgressDefaultProxy:
		name = "proxy-url"
	default:
		name = cleanText(s.Endpoint)
	}
	return Subject{Kind: SubjectProxy, ID: cleanText(s.ID), Name: name}
}

func egressProxyLabel(s EgressSample, language string) string {
	endpoint, name := cleanText(s.Endpoint), cleanText(s.Name)
	switch s.Kind {
	case EgressSharedEntry:
		return fmt.Sprintf(pick(language, "Shared proxy \"%s\" (%s)", "平台共享代理「%s」（%s）"), name, endpoint)
	case EgressAccountProxy:
		return fmt.Sprintf(pick(language, "The accounts' own proxy %s", "账号自带的代理 %s"), endpoint)
	case EgressDefaultProxy:
		return fmt.Sprintf(pick(language, "The default proxy-url %s", "默认代理 proxy-url %s"), endpoint)
	default:
		return fmt.Sprintf(pick(language, "Proxy \"%s\" (%s)", "代理「%s」（%s）"), name, endpoint)
	}
}

func egressFaultText(fault, language string) string {
	switch fault {
	case EgressFaultAuthRejected:
		return pick(language, "the proxy rejects its credentials", "代理拒绝了账号密码")
	case EgressFaultUnreachable:
		return pick(language, "the proxy cannot be reached", "连不上代理")
	default:
		return pick(language, "the proxy is not working", "代理不可用")
	}
}

func egressAccountsText(accounts int, language string) string {
	if accounts == 0 {
		return pick(language, "no account of this tenant uses it", "本租户没有账号经它出网")
	}
	return fmt.Sprintf(pick(language, "%d of this tenant's accounts use it", "本租户 %d 个账号经它出网"), accounts)
}

func describeOutage(s EgressSample, lasted time.Duration, now time.Time, language string) string {
	if language == LanguageZHCN {
		text := fmt.Sprintf("%s：%s，已熔断 %s（%s；拦下 %d 次请求，%d 次试探均失败）",
			egressProxyLabel(s, language), egressFaultText(s.Fault, language), formatDuration(lasted, language),
			egressAccountsText(s.Accounts, language), s.Refused, s.FailedProbes)
		if s.NextProbe.After(now) {
			return text + "，下次试探 " + formatInstant(s.NextProbe)
		}
		return text + "，下一个请求到来时试探"
	}
	text := fmt.Sprintf("%s: %s; requests through it have been refused for %s (%s; %d attempts refused, %d probes failed)",
		egressProxyLabel(s, language), egressFaultText(s.Fault, language), formatDuration(lasted, language),
		egressAccountsText(s.Accounts, language), s.Refused, s.FailedProbes)
	if s.NextProbe.After(now) {
		return text + ", next probe at " + formatInstant(s.NextProbe)
	}
	return text + ", the next request will probe it"
}

func describeRecovery(s EgressSample, lasted time.Duration, moved bool, language string) string {
	text := egressProxyLabel(s, language) + pick(language, " works again", " 已恢复")
	if lasted > 0 {
		text += fmt.Sprintf(pick(language, " after about %s", "，熔断约 %s"), formatDuration(lasted, language))
	}
	if moved {
		text += pick(language, " (its address was changed)", "（地址已更换）")
	}
	return text
}
