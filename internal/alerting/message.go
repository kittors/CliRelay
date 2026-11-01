package alerting

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/secretscrub"
)

// Subject kinds.
const (
	SubjectAccount = "account"
	SubjectKey     = "key"
	SubjectEndUser = "end_user"
	// SubjectChannel is kept for the one rule that is not implemented: no channel
	// stores a balance, so there is nothing to judge (see the package doc).
	SubjectChannel = "channel"
)

// Severities.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// payloadVersion is the version of the webhook payload. Fields are only ever
// added; a receiver can rely on the ones it knows.
const payloadVersion = 1

// Subject is the thing an alert is about. Name is for people, ID is what a
// receiver correlates on.
type Subject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Item is one thing a message is about. A message holds one item, or several
// when a round found enough of them that one message each would be noise.
type Item struct {
	Subject Subject `json:"subject"`
	// Facts are the numbers behind the sentence, so a receiver can act on them
	// without parsing text. Values are plain numbers, strings and RFC 3339 times.
	Facts map[string]any `json:"facts,omitempty"`
	// Text is the sentence for this item, in the message's language.
	Text string `json:"text"`
}

// Message is what gets delivered. It is also the webhook payload, which is why
// it carries no secret of any kind: no credential, no token, no request or
// response body, only names, ids and numbers the tenant already sees in its
// own panel.
type Message struct {
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	TenantID string    `json:"tenant_id"`
	Rule     string    `json:"rule"`
	Severity string    `json:"severity"`
	Language string    `json:"language"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	// Subject is the item's subject, or for a message about several items a
	// summary of them (empty ID).
	Subject Subject `json:"subject"`
	Items   []Item  `json:"items"`
}

// Plain is the message as the chat channels show it.
func (m Message) Plain() string {
	if m.Text == "" {
		return m.Title
	}
	return m.Title + "\n" + m.Text
}

// Candidate is a problem a rule found. The engine decides whether it is worth a
// message (dedupe, cooldown, grouping); the candidate only describes it.
type Candidate struct {
	Rule     string
	Severity string
	// Key identifies the problem for dedupe: the same key is the same problem,
	// and a new key (a new reset time, a new limit) is a new one.
	Key     string
	Subject Subject
	Facts   map[string]any
	// Until is when the same problem may be reported again; zero means one
	// cooldown from when it was sent.
	Until time.Time
	// Describe writes the sentence about this candidate in a language.
	Describe func(language string) string
}

// maxScrubbedText bounds one rendered field before it reaches a receiver.
const maxScrubbedText = 2000

// cleanText makes a string that came from outside the rule (an account label,
// a key name, an upstream error) safe to put into a message: control
// characters are dropped, a credential that was pasted into a name is masked,
// and the length is bounded.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(secretscrub.Scrub(s))
	return truncateRunes(s, maxScrubbedText)
}

// truncateRunes cuts s to at most limit characters, marking the cut.
func truncateRunes(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}

// cleanFacts applies cleanText to the string values of facts.
func cleanFacts(facts map[string]any) map[string]any {
	if len(facts) == 0 {
		return nil
	}
	out := make(map[string]any, len(facts))
	for k, v := range facts {
		if s, ok := v.(string); ok {
			v = cleanText(s)
		}
		out[k] = v
	}
	return out
}
