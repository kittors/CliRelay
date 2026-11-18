package modelcatalog

import (
	"net/http"
	"strings"

	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
)

const portalRootModelsPath = "/v1/models"

// PortalVisibleModelIDs returns the model IDs shared by configured availability
// and the root OpenAI-compatible GET /v1/models path. The management model
// plaza derives its tenant-visible catalog from these same two sources.
//
// opts is forwarded to the configured-availability pass so the channel-group
// editor can see models that are not yet on a group's allow-list — otherwise the
// list used to add a model is itself filtered by the setting being edited.
func (s *Service) PortalVisibleModelIDs(allowedChannelsRaw, allowedGroupsRaw string, opts ...AvailabilityFilterOptions) map[string]struct{} {
	// ConfiguredAvailability keeps disabled models so the management catalog can
	// render their toggle; every surface that actually serves models has to take
	// them back out.
	configuredIDs := modelconfigsettings.FilterOutDisabledIDs(
		s.tenantID,
		configuredAvailabilityModelIDs(s.ConfiguredAvailability(allowedChannelsRaw, allowedGroupsRaw, opts...)),
	)
	rootPathIDs := rootModelsPathIDs(s.PathAvailability(opts...))
	visible := make(map[string]struct{})
	for id := range configuredIDs {
		if _, ok := rootPathIDs[id]; ok {
			visible[id] = struct{}{}
		}
	}
	return visible
}

func configuredAvailabilityModelIDs(availability map[string]any) map[string]struct{} {
	ids := make(map[string]struct{})
	data, _ := availability["data"].([]map[string]any)
	for _, model := range data {
		if id := normalizedModelID(modelPathStringValue(model["id"])); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func rootModelsPathIDs(availability map[string]any) map[string]struct{} {
	ids := make(map[string]struct{})
	data, _ := availability["data"].([]modelPathAvailabilityResponse)
	for _, model := range data {
		for _, path := range model.Paths {
			if path.Scope == "root" && path.Method == http.MethodGet && path.Path == portalRootModelsPath {
				if id := normalizedModelID(model.ID); id != "" {
					ids[id] = struct{}{}
				}
				break
			}
		}
	}
	return ids
}

func normalizedModelID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}
