package inflight

import (
	"sort"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// modelGate serializes decisions drawing on the same key or account. It is
// separate from Ledger.mu: usage commits must be able to settle while a model
// decision is reading the database.
type modelGate struct {
	mu    sync.Mutex
	users int
}

// ModelUsage is a frozen view of the other requests that already chose a
// model. Capture it before reading persisted usage, and keep it for the whole
// decision, including fallback rules that reuse the same database snapshot.
type ModelUsage struct {
	key     map[string]Amounts
	account map[string]Amounts
}

// Matching sums each model once, even when several patterns match it.
func (u ModelUsage) Matching(scope Scope, patterns []string) Amounts {
	models := u.key
	if scope == ScopeAccount {
		models = u.account
	}
	var sum Amounts
	for model, amounts := range models {
		if quota.MatchesAnyModelPattern(patterns, model) {
			sum = sum.add(amounts)
		}
	}
	return sum
}

// BeginModelAdmission serializes model decisions for this reservation's key
// and account, and captures every other routed hold, regardless of initial
// reservation order. The caller must record the accepted model with SetModel
// before releasing the gate. Refused decisions record no model.
//
// No ledger mutex is held across the caller's database reads. Unrelated owners
// proceed independently, and unused gates are removed when their last caller
// leaves. A nil reservation needs no gate and sees no holds.
func (r *Reservation) BeginModelAdmission() (ModelUsage, func()) {
	if r == nil || r.ledger == nil {
		return ModelUsage{}, func() {}
	}
	l := r.ledger
	names := []string{r.owner.Key, r.owner.Account}
	sort.Strings(names)
	var unique []string
	for _, name := range names {
		if name != "" && (len(unique) == 0 || unique[len(unique)-1] != name) {
			unique = append(unique, name)
		}
	}
	l.mu.Lock()
	if l.modelGates == nil {
		l.modelGates = make(map[string]*modelGate)
	}
	gates := make([]*modelGate, 0, len(unique))
	for _, name := range unique {
		gate := l.modelGates[name]
		if gate == nil {
			gate = &modelGate{}
			l.modelGates[name] = gate
		}
		gate.users++
		gates = append(gates, gate)
	}
	l.mu.Unlock()
	// Every caller takes names in the same order, including callers whose key
	// changed account while an older request was still running.
	for _, gate := range gates {
		gate.mu.Lock()
	}
	l.mu.Lock()
	usage := ModelUsage{key: l.modelsLocked(r, r.owner.Key), account: l.modelsLocked(r, r.owner.Account)}
	l.mu.Unlock()
	var once sync.Once
	return usage, func() {
		once.Do(func() {
			for i := len(gates) - 1; i >= 0; i-- {
				gates[i].mu.Unlock()
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			for i, name := range unique {
				gates[i].users--
				if gates[i].users == 0 {
					delete(l.modelGates, name)
				}
			}
		})
	}
}

func (l *Ledger) modelsLocked(self *Reservation, name string) map[string]Amounts {
	var models map[string]Amounts
	for _, other := range l.byName[name] {
		if other == self || other.model == "" {
			continue
		}
		if models == nil {
			models = make(map[string]Amounts)
		}
		models[other.model] = models[other.model].add(other.amounts)
	}
	return models
}
