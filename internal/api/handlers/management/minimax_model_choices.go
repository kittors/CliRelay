package management

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// currentMiniMaxTextChoices limits discovery suggestions, not configured models
// or request routing. Keep the gateway's IDs and metadata unchanged.
func currentMiniMaxTextChoices(models []*registry.ModelInfo) []*registry.ModelInfo {
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if model != nil {
			id := strings.ToLower(model.ID)
			if strings.HasPrefix(id, "minimaxai/minimax-m") || strings.HasPrefix(id, "minimax/minimax-m") || strings.HasPrefix(id, "minimax-m") {
				switch id {
				case "minimaxai/minimax-m3", "minimax/minimax-m3.1-flash", "minimax/minimax-m3.1-flash:free", "minimax-m3.1-flash", "minimax-m3":
				default:
					continue
				}
			}
		}
		out = append(out, model)
	}
	return out
}
