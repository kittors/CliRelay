package proxy

import (
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type AssignmentEngine struct {
	mu       sync.RWMutex
	cfg      config.ProxyAssignmentConfig
	pool     []config.ProxyPoolEntry
	rrCursor int
	rand     *rand.Rand
}

func NewAssignmentEngine(cfg config.ProxyAssignmentConfig, pool []config.ProxyPoolEntry) *AssignmentEngine {
	enabled := make([]config.ProxyPoolEntry, 0, len(pool))
	for _, e := range pool {
		if e.Enabled {
			enabled = append(enabled, e)
		}
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = config.ProxyAssignmentSpread
	}
	return &AssignmentEngine{
		cfg:      cfg,
		pool:     enabled,
		rrCursor: 0,
		rand:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (e *AssignmentEngine) Assign(auths []*coreauth.Auth) []*coreauth.Auth {
	if !e.cfg.Enabled || len(e.pool) == 0 || len(auths) == 0 {
		return nil
	}

	filtered := e.filterAuths(auths)
	if len(filtered) == 0 {
		return nil
	}

	changed := make([]*coreauth.Auth, 0)
	e.mu.Lock()
	defer e.mu.Unlock()

	switch e.cfg.Strategy {
	case config.ProxyAssignmentRoundRobin:
		changed = e.assignRoundRobin(filtered)
	case config.ProxyAssignmentRandom:
		changed = e.assignRandom(filtered)
	default:
		changed = e.assignSpread(filtered)
	}

	if len(changed) > 0 {
		log.Infof("proxy assignment: %d auth entries assigned via %s strategy", len(changed), e.cfg.Strategy)
	}
	return changed
}

func (e *AssignmentEngine) filterAuths(auths []*coreauth.Auth) []*coreauth.Auth {
	out := make([]*coreauth.Auth, 0, len(auths))
	for _, a := range auths {
		if a == nil || a.Disabled {
			continue
		}
		if a.ProxyID != "" || a.ProxyURL != "" {
			continue
		}
		if len(e.cfg.Providers) > 0 {
			matched := false
			for _, p := range e.cfg.Providers {
				if a.Provider == p {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func (e *AssignmentEngine) assignSpread(auths []*coreauth.Auth) []*coreauth.Auth {
	changed := make([]*coreauth.Auth, 0)
	for i, a := range auths {
		idx := i % len(e.pool)
		proxyID := e.pool[idx].ID
		a.ProxyID = proxyID
		if a.Metadata == nil {
			a.Metadata = make(map[string]any)
		}
		a.Metadata["proxy_id"] = proxyID
		changed = append(changed, a)
	}
	return changed
}

func (e *AssignmentEngine) assignRoundRobin(auths []*coreauth.Auth) []*coreauth.Auth {
	changed := make([]*coreauth.Auth, 0)
	for _, a := range auths {
		proxyID := e.pool[e.rrCursor%len(e.pool)].ID
		e.rrCursor++
		a.ProxyID = proxyID
		if a.Metadata == nil {
			a.Metadata = make(map[string]any)
		}
		a.Metadata["proxy_id"] = proxyID
		changed = append(changed, a)
	}
	return changed
}

func (e *AssignmentEngine) assignRandom(auths []*coreauth.Auth) []*coreauth.Auth {
	changed := make([]*coreauth.Auth, 0)
	for _, a := range auths {
		idx := e.rand.Intn(len(e.pool))
		proxyID := e.pool[idx].ID
		a.ProxyID = proxyID
		if a.Metadata == nil {
			a.Metadata = make(map[string]any)
		}
		a.Metadata["proxy_id"] = proxyID
		changed = append(changed, a)
	}
	return changed
}

func (e *AssignmentEngine) Reassign(auths []*coreauth.Auth, excludeProxyID string) []*coreauth.Auth {
	if len(e.pool) == 0 {
		return nil
	}

	available := make([]config.ProxyPoolEntry, 0, len(e.pool))
	for _, p := range e.pool {
		if p.ID != excludeProxyID {
			available = append(available, p)
		}
	}
	if len(available) == 0 {
		return nil
	}

	changed := make([]*coreauth.Auth, 0)
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, a := range auths {
		if a == nil {
			continue
		}
		if a.ProxyID != excludeProxyID {
			continue
		}
		idx := e.rrCursor % len(available)
		e.rrCursor++
		newID := available[idx].ID
		a.ProxyID = newID
		if a.Metadata == nil {
			a.Metadata = make(map[string]any)
		}
		a.Metadata["proxy_id"] = newID
		changed = append(changed, a)
	}
	if len(changed) > 0 {
		log.Infof("proxy reassignment: %d auth entries moved away from proxy %s", len(changed), excludeProxyID)
	}
	return changed
}

func (e *AssignmentEngine) UpdatePool(pool []config.ProxyPoolEntry) {
	enabled := make([]config.ProxyPoolEntry, 0, len(pool))
	for _, entry := range pool {
		if entry.Enabled {
			enabled = append(enabled, entry)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.pool = enabled
}

func (e *AssignmentEngine) EnabledProxies() []config.ProxyPoolEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]config.ProxyPoolEntry, len(e.pool))
	copy(out, e.pool)
	return out
}

type ProxyDistribution struct {
	ProxyID     string   `json:"proxy_id"`
	ProxyName   string   `json:"proxy_name"`
	AuthCount   int      `json:"auth_count"`
	AuthEntries []string `json:"auth_entries,omitempty"`
}

func ComputeDistribution(auths []*coreauth.Auth, pool []config.ProxyPoolEntry) []ProxyDistribution {
	counts := make(map[string][]string)
	for _, a := range auths {
		pid := a.ProxyID
		if pid == "" {
			pid = "(none)"
		}
		counts[pid] = append(counts[pid], a.ID)
	}

	poolNames := make(map[string]string)
	for _, p := range pool {
		poolNames[p.ID] = p.Name
	}

	out := make([]ProxyDistribution, 0, len(counts))
	for pid, entries := range counts {
		name := poolNames[pid]
		if name == "" && pid != "(none)" {
			name = pid
		}
		out = append(out, ProxyDistribution{
			ProxyID:     pid,
			ProxyName:   name,
			AuthCount:   len(entries),
			AuthEntries: entries,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AuthCount > out[j].AuthCount
	})
	return out
}
