package config

// ModelCatalogs names optional local catalog files. Empty paths use the
// embedded defaults shipped with the binary.
type ModelCatalogs struct {
	Catalog      string `yaml:"catalog,omitempty" json:"catalog,omitempty"`
	CodexCatalog string `yaml:"codex-catalog,omitempty" json:"codex-catalog,omitempty"`
	DevinCatalog string `yaml:"devin-catalog,omitempty" json:"devin-catalog,omitempty"`
}

func (m ModelCatalogs) Validate() error { return nil }
