package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexcarrier"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// Carrier model wiring for Codex image generation.
//
// The resolution rules live in internal/codexcarrier; see that package for why the
// carrier cannot be a constant. This file only feeds it: the operator override
// from config, and the usable model IDs from whatever the live manifest last
// returned.

// resolveCodexImageBaseModel returns the chat model that carries image_generation.
func (e *CodexExecutor) resolveCodexImageBaseModel() string {
	if e != nil && e.cfg != nil {
		codexcarrier.SetOverride(e.cfg.CodexImageBaseModel)
	}
	publishCodexCarrierCandidates(loadCodexModels())
	return codexcarrier.Resolve()
}

// publishCodexCarrierCandidates forwards manifest model IDs to the carrier
// resolver. A manifest that has not been warmed yet publishes nothing, leaving any
// previously published list in place rather than blanking it on a cold read.
func publishCodexCarrierCandidates(models []*sdkmodelcatalog.ModelInfo) {
	if len(models) == 0 {
		return
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		ids = append(ids, model.ID)
	}
	if len(ids) == 0 {
		return
	}
	codexcarrier.Publish(ids)
}
