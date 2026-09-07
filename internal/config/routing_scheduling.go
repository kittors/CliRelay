package config

import (
	"math"
	"strings"
)

// Distribution modes decide which candidate a group hands the request to once
// the candidate set is known. They are orthogonal to session stickiness: sticky
// only pins an already-running conversation, the distribution still decides
// where every *new* conversation lands and where a released binding goes next.
const (
	DistributionWeighted  = "weighted"
	DistributionLeastLoad = "least-load"
	DistributionFillFirst = "fill-first"
)

// DefaultStickyMaxRequests bounds how many requests one conversation may pin to
// a single account. Without a bound a long session keeps hammering the same
// upstream account until it hits a 429, which is exactly the "burn one account"
// behaviour this model replaces.
const DefaultStickyMaxRequests = 200

// GroupStickyConfig configures session stickiness as an independent switch.
type GroupStickyConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// MaxRequests forces a re-pick after this many requests on the same account.
	// Zero means "use DefaultStickyMaxRequests"; a negative value means unbounded.
	MaxRequests int `yaml:"max-requests,omitempty" json:"max-requests,omitempty"`

	// ReleaseAtLoad releases the binding early once the bound account's load
	// ratio (0..1 of its quota window) reaches this value, before the account is
	// actually exhausted. Zero disables the early release.
	ReleaseAtLoad float64 `yaml:"release-at-load,omitempty" json:"release-at-load,omitempty"`
}

// GroupScheduling describes how one channel group distributes requests.
type GroupScheduling struct {
	Distribution string            `yaml:"distribution,omitempty" json:"distribution,omitempty"`
	Sticky       GroupStickyConfig `yaml:"sticky,omitempty" json:"sticky,omitempty"`

	// ChannelWeights carries the relative share per channel. Absent entries mean
	// weight 1 (matching the panel copy), an explicit 0 means "do not schedule".
	ChannelWeights map[string]int `yaml:"channel-weights,omitempty" json:"channel-weights,omitempty"`
}

// NormalizeDistribution maps user input onto a supported distribution mode.
func NormalizeDistribution(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "least-load", "leastload", "ll":
		return DistributionLeastLoad
	case "fill-first", "fillfirst", "ff":
		return DistributionFillFirst
	default:
		return DistributionWeighted
	}
}

// normalizeChannelWeights drops blank names and negative weights. Unlike the
// legacy priority map, 0 is preserved: it is the explicit "exclude" value.
func normalizeChannelWeights(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]int, len(values))
	for key, weight := range values {
		name := strings.TrimSpace(key)
		if name == "" || weight < 0 {
			continue
		}
		if existing, exists := out[name]; exists && existing >= weight {
			continue
		}
		out[name] = weight
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// legacyStrategyForScheduling derives the pre-scheduling `strategy` value so a
// config written by this build still reads sensibly on an older binary and in
// an older admin panel. least-load has no legacy equivalent and degrades to
// round-robin, which is the closest balancing behaviour.
func legacyStrategyForScheduling(scheduling GroupScheduling) string {
	if scheduling.Sticky.Enabled {
		return "session-sticky"
	}
	if scheduling.Distribution == DistributionFillFirst {
		return "fill-first"
	}
	return "round-robin"
}

// MigrateLegacyPriorities converts an old `channel-priorities` map into weights.
//
// The old map was ambiguous: under round-robin a 0 already meant "excluded",
// but under session-sticky and fill-first it meant "the default tier", which is
// where every unconfigured channel sat. A map that is entirely zeros can only
// have come from the second reading — nobody configures a group that can never
// serve a request — so it is dropped and every member falls back to weight 1.
// Mixed maps keep their zeros, since there a 0 was already an exclusion
// relative to the higher-valued entries.
func MigrateLegacyPriorities(priorities map[string]int) map[string]int {
	if len(priorities) == 0 {
		return nil
	}
	for _, priority := range priorities {
		if priority > 0 {
			return priorities
		}
	}
	return nil
}

// SchedulingFromLegacyGroup derives the scheduling block for a group that has
// not been migrated yet, so read paths (management API, runtime snapshot) show
// the same effective configuration the selector will use.
func SchedulingFromLegacyGroup(group RoutingChannelGroup) GroupScheduling {
	scheduling := schedulingFromLegacy(group.Strategy, group.ChannelPriorities)
	if scheduling.Sticky.Enabled && scheduling.Sticky.MaxRequests == 0 {
		scheduling.Sticky.MaxRequests = DefaultStickyMaxRequests
	}
	return scheduling
}

// schedulingFromLegacy upgrades a group that only carries the old `strategy` +
// `channel-priorities` pair.
func schedulingFromLegacy(strategy string, priorities map[string]int) GroupScheduling {
	scheduling := GroupScheduling{ChannelWeights: MigrateLegacyPriorities(priorities)}
	switch NormalizeRoutingStrategy(strategy) {
	case "session-sticky":
		scheduling.Distribution = DistributionWeighted
		scheduling.Sticky.Enabled = true
	case "fill-first":
		scheduling.Distribution = DistributionFillFirst
	default:
		scheduling.Distribution = DistributionWeighted
	}
	return scheduling
}

// sanitizeScheduling normalizes one group's scheduling block and keeps the
// legacy fields mirrored so both readers stay consistent during the rollout.
func sanitizeScheduling(group *RoutingChannelGroup) {
	if group == nil {
		return
	}
	scheduling := group.Scheduling
	migrated := false
	if strings.TrimSpace(scheduling.Distribution) == "" && !scheduling.Sticky.Enabled && len(scheduling.ChannelWeights) == 0 {
		scheduling = schedulingFromLegacy(group.Strategy, group.ChannelPriorities)
		migrated = true
	}

	scheduling.Distribution = NormalizeDistribution(scheduling.Distribution)
	scheduling.ChannelWeights = normalizeChannelWeights(scheduling.ChannelWeights)
	if !migrated && len(scheduling.ChannelWeights) == 0 && len(group.ChannelPriorities) > 0 {
		// The group gained a scheduling block but kept its weights in the legacy
		// map; carry them over instead of silently resetting everyone to 1.
		scheduling.ChannelWeights = normalizeChannelWeights(MigrateLegacyPriorities(group.ChannelPriorities))
	}

	if !scheduling.Sticky.Enabled {
		scheduling.Sticky = GroupStickyConfig{}
	} else {
		if scheduling.Sticky.MaxRequests == 0 {
			scheduling.Sticky.MaxRequests = DefaultStickyMaxRequests
		}
		if scheduling.Sticky.MaxRequests < 0 {
			scheduling.Sticky.MaxRequests = 0 // explicit "unbounded"
		}
		if scheduling.Sticky.ReleaseAtLoad < 0 || math.IsNaN(scheduling.Sticky.ReleaseAtLoad) {
			scheduling.Sticky.ReleaseAtLoad = 0
		}
		if scheduling.Sticky.ReleaseAtLoad > 1 {
			scheduling.Sticky.ReleaseAtLoad = 1
		}
	}

	group.Scheduling = scheduling
	group.Strategy = legacyStrategyForScheduling(scheduling)
	group.ChannelPriorities = normalizeChannelWeights(scheduling.ChannelWeights)
}

// ExplicitWeightsAllZero reports whether every *explicitly configured* weight
// excludes its channel. Callers must combine it with the group's resolved
// membership: channels without an entry default to weight 1, so a group is only
// truly unusable when every member is covered by a zero entry. Validating this
// at save time avoids the opaque runtime "auth_unavailable" the old code
// produced when a whole group was set to 0.
func ExplicitWeightsAllZero(scheduling GroupScheduling) bool {
	if len(scheduling.ChannelWeights) == 0 {
		return false
	}
	for _, weight := range scheduling.ChannelWeights {
		if weight > 0 {
			return false
		}
	}
	return true
}
