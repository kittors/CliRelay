// Package patchfield holds JSON field types for management PATCH bodies.
package patchfield

import (
	"bytes"
	"encoding/json"
)

// NullableBool is a PATCH field for a tri-state setting: an explicit true or
// false override, or no override at all. Leaving the key out keeps the stored
// value, JSON null clears the override, and true or false sets it. A plain
// *bool cannot tell null from a missing key, so it could never clear one.
type NullableBool struct {
	// Set reports whether the key was present in the body.
	Set bool
	// Value is the override to store; nil clears it.
	Value *bool
}

// UnmarshalJSON records that the key was present. encoding/json calls it for
// an explicit null too, which is what lets null mean "clear".
func (b *NullableBool) UnmarshalJSON(data []byte) error {
	b.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		b.Value = nil
		return nil
	}
	var value bool
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	b.Value = &value
	return nil
}

// Apply writes the patch onto target when the key was present and reports
// whether it did.
func (b NullableBool) Apply(target **bool) bool {
	if !b.Set || target == nil {
		return false
	}
	if b.Value == nil {
		*target = nil
		return true
	}
	value := *b.Value
	*target = &value
	return true
}
