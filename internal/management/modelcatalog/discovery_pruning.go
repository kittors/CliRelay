package modelcatalog

import "github.com/router-for-me/CLIProxyAPI/v6/internal/registry"

// survivesDiscoveryPruning reports whether a model must be kept even when a
// provider's live listing does not mention it.
//
// The management views prune static models absent from discovery so operators are
// not offered models a provider has retired. That rule is right for chat models
// and wrong for image models: generation is served by a separate endpoint and
// never appears in a chat listing, so absence there carries no availability
// signal. Pruning them is why gpt-image-2 was missing from the model plaza, the
// catalog and the channel-group editor while the credential detail listed it —
// and, because the editor is where a model is added to a group's allow-list, why
// it could not be permitted either.
func survivesDiscoveryPruning(modelKey string) bool {
	return registry.IsImageGenerationModel(modelKey)
}
