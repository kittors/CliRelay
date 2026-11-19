package accessfreeze

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// registry is this process's copy of the active freezes. Every authenticated
// request reads it, so a requester's notices are encoded once, when its
// freezes change, and the request path only looks a string up.
type registry struct {
	mu        sync.RWMutex
	loc       *time.Location
	bySubject map[string]map[string]Freeze
	encoded   map[string]string
	// changes counts this process's own puts, so a full load read before one
	// of them cannot overwrite it (see replaceAllUnlessChanged).
	changes uint64
}

func newRegistry(loc *time.Location) *registry {
	if loc == nil {
		loc = time.Local
	}
	return &registry{loc: loc, bySubject: map[string]map[string]Freeze{}, encoded: map[string]string{}}
}

// generation is the number of puts so far; pass it to replaceAllUnlessChanged.
func (r *registry) generation() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.changes
}

// replaceAllUnlessChanged installs a full load read after generation was
// taken, unless this process froze or released something meanwhile: the load
// may predate that change, and installing it would undo the change until the
// next refresh. It reports whether the load was installed.
func (r *registry) replaceAllUnlessChanged(generation uint64, freezes []Freeze) bool {
	bySubject, encoded := r.index(freezes)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changes != generation {
		return false
	}
	r.bySubject = bySubject
	r.encoded = encoded
	return true
}

func (r *registry) index(freezes []Freeze) (map[string]map[string]Freeze, map[string]string) {
	bySubject := make(map[string]map[string]Freeze, len(freezes))
	for _, f := range freezes {
		if !f.Active() || !f.Subject.valid() {
			continue
		}
		key := f.Subject.key()
		if bySubject[key] == nil {
			bySubject[key] = map[string]Freeze{}
		}
		bySubject[key][normalizeProvider(f.Provider)] = f
	}
	encoded := make(map[string]string, len(bySubject))
	for key, byProvider := range bySubject {
		encoded[key] = r.encode(byProvider)
	}
	return bySubject, encoded
}

// put records an active freeze, or drops a released one.
func (r *registry) put(f Freeze) {
	if !f.Subject.valid() {
		return
	}
	key := f.Subject.key()
	provider := normalizeProvider(f.Provider)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes++
	byProvider := r.bySubject[key]
	if f.Active() {
		if byProvider == nil {
			byProvider = map[string]Freeze{}
			r.bySubject[key] = byProvider
		}
		byProvider[provider] = f
	} else if current, ok := byProvider[provider]; ok && current.ID == f.ID {
		delete(byProvider, provider)
	}
	if len(byProvider) == 0 {
		delete(r.bySubject, key)
		delete(r.encoded, key)
		return
	}
	r.encoded[key] = r.encode(byProvider)
}

// active returns the subject's freeze of provider, if there is one.
func (r *registry) active(subject Subject, provider string) (Freeze, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.bySubject[subject.key()][normalizeProvider(provider)]
	return f, ok
}

// frozenProviders is the value of the access metadata's frozen-providers key
// for subject: a JSON object from provider to notice, or "".
func (r *registry) frozenProviders(subject Subject) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.encoded[subject.key()]
}

// list returns the subject's active freezes, oldest first.
func (r *registry) list(subject Subject) []Freeze {
	r.mu.RLock()
	byProvider := r.bySubject[subject.key()]
	out := make([]Freeze, 0, len(byProvider))
	for _, f := range byProvider {
		out = append(out, f)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].FrozenAt.Before(out[j].FrozenAt) })
	return out
}

func (r *registry) encode(byProvider map[string]Freeze) string {
	notices := make(map[string]string, len(byProvider))
	for provider, f := range byProvider {
		notices[provider] = Notice(f, r.loc)
	}
	raw, err := json.Marshal(notices)
	if err != nil {
		return ""
	}
	return string(raw)
}
