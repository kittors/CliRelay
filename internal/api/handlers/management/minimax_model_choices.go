package management

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// currentMiniMaxTextChoices limits discovery suggestions, not configured models
// or request routing. Keep the gateway's IDs and metadata unchanged.
func currentMiniMaxTextChoices(models []*registry.ModelInfo) []*registry.ModelInfo {
	out := make([]*registry.ModelInfo, 0, len(models))
	var slots []int
	var flash, m3 []*registry.ModelInfo
	for _, model := range models {
		if model != nil {
			id := strings.ToLower(model.ID)
			if strings.HasPrefix(id, "minimaxai/minimax-m") || strings.HasPrefix(id, "minimax/minimax-m") || strings.HasPrefix(id, "minimax-m") {
				switch id {
				case "minimaxai/minimax-m3", "minimax/minimax-m3.1-flash", "minimax/minimax-m3.1-flash:free", "minimax-m3.1-flash", "minimax-m3":
				default:
					continue
				}
				slots = append(slots, len(out))
				if strings.Contains(id, "m3.1-flash") {
					flash = append(flash, model)
				} else {
					m3 = append(m3, model)
				}
			}
		}
		out = append(out, model)
	}
	// Reorder only MiniMax slots so other providers retain their positions.
	current := append(flash, m3...)
	for i, slot := range slots {
		out[slot] = current[i]
	}
	return out
}
