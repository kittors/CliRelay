package contentmoderation

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const ginWordGuardSnapshotKey = "content_moderation_word_guard_snapshot"

// WordGuardSnapshot is the word guard's outcome for one request, stored with
// the request's details in the request log.
//
// The request side is settled before the request runs, but the reply side keeps
// counting while the model is still answering, from the goroutine that filters
// the stream; the log is written from another. The snapshot therefore guards
// its fields and is only ever read through MarshalJSON.
type WordGuardSnapshot struct {
	mu   sync.Mutex
	view wordGuardView
}

// wordGuardView is what is persisted.
type wordGuardView struct {
	// Action is allow, log, replace or block: the strongest thing any hit led
	// to on the way in.
	Action  string `json:"action"`
	Blocked bool   `json:"blocked"`
	// Reason is set when the request was blocked for another reason than a hit
	// under the block action.
	Reason string `json:"reason,omitempty"`
	// Layers lists the policy layers whose rules were applied.
	Layers     []string    `json:"layers,omitempty"`
	MatchCount int         `json:"match_count"`
	Replaced   int         `json:"replaced"`
	Matches    []WordMatch `json:"matches,omitempty"`
	LatencyMS  int64       `json:"latency_ms"`
	// Field names the text a replace could not rewrite, when Reason says so.
	Field string `json:"field,omitempty"`
	// RegexSkipped says some regular expressions were not run on this request
	// because it is larger than the work they are allowed (see regexBudget): a
	// failed open.
	RegexSkipped bool `json:"regex_skipped,omitempty"`
	// PolicyError is set when the policies could not be read from the database
	// in time (policy_store_timeout) or at all (policy_store_error), and the
	// guard worked from the ones it saw last, or none: a failed open.
	PolicyError string `json:"policy_error,omitempty"`
	// Reply is present once the reply filter has found a hit or could not do its
	// work.
	Reply *WordGuardReply `json:"reply,omitempty"`
}

// Reasons part of a reply was left unfiltered, besides the ones the codec names.
const (
	// ReplySkipRegexBudget: the reply is longer than the work the regular
	// expressions are allowed, so they stopped being run on it.
	ReplySkipRegexBudget = "regex_budget"
)

// WordGuardReply is what the reply filter did.
type WordGuardReply struct {
	// Hits counts the words found in the reply, Replaced the ones that were
	// replaced (a log-only rule finds without replacing).
	Hits     int `json:"hits"`
	Replaced int `json:"replaced"`
	// Skipped says why part of the reply was not filtered, e.g. text that is
	// not valid UTF-8 or a compressed body it could not read.
	Skipped string `json:"skipped,omitempty"`
}

// newWordGuardSnapshot describes a request-side verdict.
func newWordGuardSnapshot(verdict WordVerdict, contexts []wordContext, policyError string) *WordGuardSnapshot {
	view := wordGuardView{
		Action:       verdict.Action,
		Blocked:      verdict.Blocked,
		Reason:       verdict.Reason,
		MatchCount:   verdict.MatchCount,
		Replaced:     verdict.Replaced,
		Matches:      verdict.Matches,
		LatencyMS:    verdict.LatencyMS,
		Field:        verdict.Field,
		RegexSkipped: verdict.RegexSkipped,
		PolicyError:  policyError,
	}
	for _, context := range contexts {
		view.Layers = append(view.Layers, context.layer)
	}
	return &WordGuardSnapshot{view: view}
}

// addReply counts what the reply filter did to one piece of the reply.
func (s *WordGuardSnapshot) addReply(hits, replaced int) {
	if s == nil || (hits == 0 && replaced == 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply().Hits += hits
	s.reply().Replaced += replaced
}

// skipReply notes that part of the reply could not be filtered. The first
// reason is kept.
func (s *WordGuardSnapshot) skipReply(reason string) {
	if s == nil || reason == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if reply := s.reply(); reply.Skipped == "" {
		reply.Skipped = reason
	}
}

func (s *WordGuardSnapshot) reply() *WordGuardReply {
	if s.view.Reply == nil {
		s.view.Reply = &WordGuardReply{}
	}
	return s.view.Reply
}

// Notable says whether the snapshot is worth a place in the request log. A
// request the guard looked at and found nothing in says nothing, and the log
// pays for every byte it holds.
func (s *WordGuardSnapshot) Notable() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view.MatchCount > 0 || s.view.Blocked || s.view.PolicyError != "" || s.view.RegexSkipped || s.view.Reply != nil
}

// MarshalJSON writes a consistent copy while the reply may still be counting.
func (s *WordGuardSnapshot) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	view := s.view
	if view.Reply != nil {
		reply := *view.Reply
		view.Reply = &reply
	}
	s.mu.Unlock()
	return json.Marshal(view)
}

// SetWordGuardSnapshot attaches the guard's outcome to the request's Gin context.
func SetWordGuardSnapshot(ctx context.Context, snapshot *WordGuardSnapshot) {
	if ctx == nil || snapshot == nil {
		return
	}
	if ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context); ginCtx != nil {
		ginCtx.Set(ginWordGuardSnapshotKey, snapshot)
	}
}

// WordGuardSnapshotFromGin returns the request-scoped word guard outcome, if
// there is one worth recording.
func WordGuardSnapshotFromGin(ginCtx *gin.Context) (*WordGuardSnapshot, bool) {
	if ginCtx == nil {
		return nil, false
	}
	value, exists := ginCtx.Get(ginWordGuardSnapshotKey)
	if !exists {
		return nil, false
	}
	snapshot, ok := value.(*WordGuardSnapshot)
	return snapshot, ok && snapshot.Notable()
}
