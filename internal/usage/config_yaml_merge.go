package usage

import (
	"bytes"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// MergeDBSettingsIntoYAML takes raw YAML content and the current in-memory config
// (which has DB-backed settings applied), then injects the DB-backed sections into
// the YAML document so the management panel can read them.
// Returns the merged YAML bytes, or the original data if merging is not needed or fails.
func MergeDBSettingsIntoYAML(data []byte, cfg *config.Config) []byte {
	if cfg == nil || !ConfigStoreAvailable() {
		return data
	}

	// Parse the original YAML document.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		log.Warnf("usage: merge DB settings: failed to parse YAML: %v", err)
		return data
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return data
	}
	root := doc.Content[0]
	if root == nil || root.Kind != yaml.MappingNode {
		return data
	}

	// Marshal the full config (with DB settings applied) to YAML, then parse it.
	rendered, err := yaml.Marshal(cfg)
	if err != nil {
		log.Warnf("usage: merge DB settings: failed to marshal config: %v", err)
		return data
	}
	var fullDoc yaml.Node
	if err := yaml.Unmarshal(rendered, &fullDoc); err != nil {
		log.Warnf("usage: merge DB settings: failed to parse rendered config: %v", err)
		return data
	}
	if fullDoc.Kind != yaml.DocumentNode || len(fullDoc.Content) == 0 {
		return data
	}
	fullRoot := fullDoc.Content[0]
	if fullRoot == nil || fullRoot.Kind != yaml.MappingNode {
		return data
	}

	// Build a lookup of key -> value node from the full rendered config.
	fullKeys := make(map[string]*yaml.Node, len(fullRoot.Content)/2)
	for i := 0; i+1 < len(fullRoot.Content); i += 2 {
		keyNode := fullRoot.Content[i]
		valNode := fullRoot.Content[i+1]
		if keyNode.Kind == yaml.ScalarNode {
			fullKeys[keyNode.Value] = valNode
		}
	}

	// For each DB-backed key, inject or replace it in the original document.
	for key := range dbBackedConfigYAMLKeys {
		valNode, exists := fullKeys[key]
		if !exists {
			continue
		}
		// Skip empty/null values to avoid injecting noise.
		if isEmptyYAMLValue(valNode) {
			continue
		}
		injectOrReplaceKey(root, key, valNode)
	}

	// Re-encode the merged document.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		log.Warnf("usage: merge DB settings: failed to encode merged YAML: %v", err)
		return data
	}
	_ = enc.Close()
	return buf.Bytes()
}

// injectOrReplaceKey sets or replaces a root-level key in a mapping node.
func injectOrReplaceKey(root *yaml.Node, key string, value *yaml.Node) {
	// Try to find and replace existing key.
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Kind == yaml.ScalarNode && root.Content[i].Value == key {
			root.Content[i+1] = value
			return
		}
	}
	// Key doesn't exist, append it.
	keyNode := &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: key,
	}
	root.Content = append(root.Content, keyNode, value)
}

// isEmptyYAMLValue checks if a YAML value node is effectively empty.
func isEmptyYAMLValue(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Tag == "!!null" || node.Value == "" || node.Value == "null"
	case yaml.SequenceNode:
		return len(node.Content) == 0
	case yaml.MappingNode:
		return len(node.Content) == 0
	}
	return false
}
