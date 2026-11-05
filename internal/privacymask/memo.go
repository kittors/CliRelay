package privacymask

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// memoRules remembers what the rules found in a long text. An agent sends
// its whole history on every turn, so without it every turn ran each rule
// over every message before it again: on a long Claude Code session, with
// masking on, most of magpie's CPU went to the rules' regexps (#4353). What
// they find in a text depends on the text, what o covers, the user's own
// rules and the day (dateOK: a date over a year ago can be a birthday),
// and on nothing a request changes, so it is kept by a hash of those. Only
// the hash and the places found are kept, nothing of a value.
const (
	ruleMemoMin     = 256 // a shorter text costs less to read than to look up
	ruleMemoEntries = 1 << 16
)

var ruleMemo struct {
	sync.Mutex
	cur, old map[[32]byte][]span
}

// ruleRuns counts the texts of ruleMemoMin bytes or more the rules have
// run over (a test reads it).
var ruleRuns atomic.Uint64

func memoRules(s string, o Options, find func() []span) []span {
	if len(s) < ruleMemoMin {
		return find()
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%t\x00%t\x00", dateClock().Format("2006-01-02"), o.Secrets, o.Personal)
	kinds := make([]string, 0, len(o.Kinds))
	for k, on := range o.Kinds {
		kinds = append(kinds, fmt.Sprintf("%q=%t", k, on))
	}
	slices.Sort(kinds)
	fmt.Fprintf(h, "%s\x00", strings.Join(kinds, ","))
	if o.Secrets {
		for _, r := range o.Rules {
			fmt.Fprintf(h, "%q:%q:%q;", r.Kind, r.Prefix, r.Regex)
		}
	}
	h.Write([]byte{0})
	h.Write([]byte(s))
	var k [32]byte
	h.Sum(k[:0])
	ruleMemo.Lock()
	sp, ok := ruleMemo.cur[k]
	if !ok {
		if sp, ok = ruleMemo.old[k]; ok {
			ruleAdd(k, sp) // still in use: kept past the next turnover
		}
	}
	ruleMemo.Unlock()
	if ok {
		return slices.Clone(sp)
	}
	sp = find()
	ruleMemo.Lock()
	ruleAdd(k, slices.Clip(slices.Clone(sp)))
	ruleMemo.Unlock()
	return sp
}

// ruleAdd is called with ruleMemo held. Two generations, as sentMemo
// keeps: when the newer is full it becomes the older.
func ruleAdd(k [32]byte, sp []span) {
	if ruleMemo.cur == nil || len(ruleMemo.cur) >= ruleMemoEntries {
		ruleMemo.old, ruleMemo.cur = ruleMemo.cur, make(map[[32]byte][]span)
	}
	ruleMemo.cur[k] = sp
}
