// Package accessfreeze holds requesters back from a provider after that
// provider's upstream refused one of their requests for its content.
//
// An upstream safety refusal (Codex's cyber_policy) is the upstream judging
// the requester's content, and the accounts it judges it on are the pool's,
// shared by every tenant user. Once a requester reaches the threshold, the
// gateway freezes that requester's access to the provider, only that one: the
// requester's other providers keep working. Selection leaves the provider's
// credentials out of the requester's requests and answers with a notice that
// says why (see Notice). An administrator lifts a freeze by hand.
package accessfreeze

import (
	"time"
	"unicode/utf8"
)

// SubjectKind names what a freeze holds back.
type SubjectKind string

const (
	// SubjectEndUser is a portal account: every API key it owns is held back.
	SubjectEndUser SubjectKind = "end_user"
	// SubjectAPIKey is an API key no portal account owns.
	SubjectAPIKey SubjectKind = "api_key"
)

// Valid reports whether k is a known subject kind.
func (k SubjectKind) Valid() bool {
	return k == SubjectEndUser || k == SubjectAPIKey
}

// Subject is the requester a freeze applies to.
type Subject struct {
	TenantID string      `json:"tenant_id"`
	Kind     SubjectKind `json:"kind"`
	ID       string      `json:"id"`
}

func (s Subject) valid() bool {
	return s.TenantID != "" && s.Kind.Valid() && s.ID != ""
}

func (s Subject) key() string {
	return s.TenantID + "|" + string(s.Kind) + "|" + s.ID
}

// Freeze holds a subject back from one provider. AuthLabel names the upstream
// account that refused the request: administrators see it, a portal user never
// does (the portal does not reveal channel identity).
type Freeze struct {
	ID              int64      `json:"id"`
	Subject         Subject    `json:"subject"`
	Provider        string     `json:"provider"`
	ReasonCode      string     `json:"reason_code"`
	UpstreamMessage string     `json:"upstream_message"`
	Model           string     `json:"model"`
	AuthLabel       string     `json:"auth_label,omitempty"`
	RequestID       string     `json:"request_id,omitempty"`
	StrikeCount     int        `json:"strike_count"`
	FrozenAt        time.Time  `json:"frozen_at"`
	ReleasedAt      *time.Time `json:"released_at,omitempty"`
	ReleasedBy      string     `json:"released_by,omitempty"`
	ReleaseNote     string     `json:"release_note,omitempty"`
}

// Active reports whether the freeze still holds.
func (f Freeze) Active() bool {
	return f.ReleasedAt == nil
}

// Strike is one upstream content refusal of a subject's request: the evidence
// a freeze rests on, and what the threshold counts.
type Strike struct {
	ID              int64     `json:"id"`
	Subject         Subject   `json:"subject"`
	Provider        string    `json:"provider"`
	ReasonCode      string    `json:"reason_code"`
	UpstreamMessage string    `json:"upstream_message"`
	Model           string    `json:"model"`
	AuthLabel       string    `json:"auth_label,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// maxUpstreamMessage caps the upstream message kept with a strike or freeze.
// Codex's cyber_policy message is about 330 bytes; the cap only guards the
// table against an upstream that echoes the request back.
const maxUpstreamMessage = 2000

func capUpstreamMessage(message string) string {
	if len(message) <= maxUpstreamMessage {
		return message
	}
	cut := maxUpstreamMessage
	// Back up to a rune start so the cut never splits a character.
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut]
}
