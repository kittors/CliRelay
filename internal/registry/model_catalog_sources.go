package registry

import (
	"context"
	"os"
	"sync"
)

// ModelCatalogSources is the registry-facing form of configured catalog paths.
// It intentionally avoids importing the config package, which already depends
// on registry model metadata types.
type ModelCatalogSources struct {
	Catalog      string
	CodexCatalog string
	DevinCatalog string
}

var (
	modelCatalogMu       sync.Mutex
	modelRefreshCallback func([]string)
)

// SetModelRefreshCallback installs a test and embedding hook for catalog
// changes.
func SetModelRefreshCallback(callback func([]string)) {
	modelCatalogMu.Lock()
	modelRefreshCallback = callback
	modelCatalogMu.Unlock()
}

func notifyModelRefresh(providers ...string) {
	modelCatalogMu.Lock()
	callback := modelRefreshCallback
	modelCatalogMu.Unlock()
	if callback != nil {
		callback(providers)
	}
}

// UpdateModelCatalogSources applies configured catalogs immediately.
func UpdateModelCatalogSources(sources ModelCatalogSources, _ bool) {
	if sources.DevinCatalog != "" {
		if data, err := os.ReadFile(sources.DevinCatalog); err == nil {
			if changed, errLoad := loadDevinModelsFromBytes(data, sources.DevinCatalog); errLoad == nil && changed {
				notifyModelRefresh("devin")
			}
		}
	}
}

// StartModelCatalogUpdaters loads configured catalogs and keeps the operation
// bound to the caller's lifetime. The local source is read at startup; config
// reloads call UpdateModelCatalogSources for subsequent changes.
func StartModelCatalogUpdaters(ctx context.Context, sources ModelCatalogSources, home bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	UpdateModelCatalogSources(sources, home)
}
