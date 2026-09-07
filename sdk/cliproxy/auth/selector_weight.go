package auth

import (
	"strconv"
	"strings"
)

// selectionWeightAttribute carries the effective per-candidate weight resolved
// from the routing group. It replaces the overloaded "priority" attribute,
// which used to mean "relative share" under round-robin but "hard tier cutoff"
// under session-sticky / fill-first — the same number produced opposite
// routing decisions depending on the group's strategy.
const selectionWeightAttribute = "selection_weight"

// legacySelectionWeightAttribute is still honoured so credentials that carry a
// hand-written attribute (there has never been a panel input for it) keep
// working. It is only consulted when the routing group does not resolve a
// weight for the candidate.
const legacySelectionWeightAttribute = "priority"

// defaultSelectionWeight matches the documented panel behaviour: a channel with
// no configured weight participates with share 1.
const defaultSelectionWeight = 1

func parseWeightAttribute(auth *Auth, key string) (int, bool) {
	if auth == nil || auth.Attributes == nil {
		return 0, false
	}
	raw := strings.TrimSpace(auth.Attributes[key])
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// authSelectionWeight returns the relative share for a candidate.
//
// Semantics (uniform across every distribution mode):
//   - unset      -> defaultSelectionWeight (1)
//   - n > 0      -> relative share
//   - 0 or below -> excluded from scheduling
func authSelectionWeight(auth *Auth) int {
	if weight, ok := parseWeightAttribute(auth, selectionWeightAttribute); ok {
		if weight <= 0 {
			return 0
		}
		return weight
	}

	// The legacy "priority" attribute is read with its own historical meaning:
	// it was a tier, where 0 was simply the default tier every unconfigured
	// credential landed in — not an exclusion. Interpreting a stored 0 as the
	// new "weight 0 = do not schedule" would silently take existing accounts
	// out of rotation on upgrade, so only positive legacy values carry over.
	if weight, ok := parseWeightAttribute(auth, legacySelectionWeightAttribute); ok && weight > 0 {
		return weight
	}
	return defaultSelectionWeight
}

// authParticipatesInSelection reports whether the candidate may receive traffic.
func authParticipatesInSelection(auth *Auth) bool {
	return authSelectionWeight(auth) > 0
}
