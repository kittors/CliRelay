package usagelogs

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func canonicalChannelAuthIndex(authIndex string, authIndexGroup map[string][]string) string {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return ""
	}
	if group := authIndexGroup[authIndex]; len(group) > 0 {
		// buildNameMaps stores the live EnsureIndex() first.
		return strings.TrimSpace(group[0])
	}
	return authIndex
}

// queryDistinctChannelRows groups by historical subject first. Record how many
// distinct facet identities land on each canonical auth_index so a subject
// rekey can collapse even when the live alias map no longer has it.
func channelFacetIdentitiesByAuthIndex(
	options []usage.ChannelFilterOption,
	authIndexGroup map[string][]string,
) map[string]map[string]struct{} {
	identitiesByIndex := make(map[string]map[string]struct{})
	for _, option := range options {
		authIndex := strings.TrimSpace(option.AuthIndex)
		if authIndex == "" && !looksLikeAuthSubjectID(option.Value) {
			authIndex = strings.TrimSpace(option.Value)
		}
		authIndex = canonicalChannelAuthIndex(authIndex, authIndexGroup)
		if authIndex == "" {
			continue
		}

		identity := strings.TrimSpace(option.AuthSubjectID)
		if identity == "" {
			if value := strings.TrimSpace(option.Value); looksLikeAuthSubjectID(value) {
				identity = value
			} else {
				identity = "<index-only>"
			}
		}
		if identitiesByIndex[authIndex] == nil {
			identitiesByIndex[authIndex] = make(map[string]struct{})
		}
		identitiesByIndex[authIndex][strings.ToLower(identity)] = struct{}{}
	}
	return identitiesByIndex
}
