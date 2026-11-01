package management

import (
	"encoding/json"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSharedAccountIdentityMasksNestedRestrictions(t *testing.T) {
	a := &coreauth.Auth{ID: "claude-owner@example.com.json", Provider: "claude", Label: "owner@example.com", Metadata: map[string]any{"email": "owner@example.com"}, Attributes: map[string]string{"runtime_only": "true"}, Status: coreauth.StatusError, StatusMessage: "account owner@example.com blocked", Quota: coreauth.QuotaState{Exceeded: true, Reason: "owner@example.com quota exceeded"}, ModelStates: map[string]*coreauth.ModelState{"model": {Status: coreauth.StatusError, LastError: &coreauth.Error{Message: "owner@example.com model blocked"}}}}
	entries := sharedAuthFileEntries([]*coreauth.Auth{a})
	if len(entries) != 1 {
		t.Fatalf("entries=%v", entries)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "owner@example.com") {
		t.Fatalf("shared identity leaked: %s", encoded)
	}
	restrictions, ok := entries[0]["restrictions"].([]map[string]any)
	if !ok || len(restrictions) != 2 {
		t.Fatalf("restrictions lost: %v", entries[0]["restrictions"])
	}
	if a.StatusMessage != "account owner@example.com blocked" || a.ModelStates["model"].LastError.Message != "owner@example.com model blocked" {
		t.Fatal("owner state was mutated")
	}
}

func TestSharedAccountIdentityCopiesNestedContainers(t *testing.T) {
	a := &coreauth.Auth{ID: "claude-owner@example.com.json", Provider: "claude", Metadata: map[string]any{"email": "owner@example.com"}}
	nested := map[string]any{"message": "owner@example.com", "count": 3}
	tags := []string{"owner@example.com", "public"}
	items := []any{nested, tags}
	entry := map[string]any{"restrictions": []map[string]any{nested}, "custom_tags": tags, "other": items}
	hideSharedAccountIdentity(entry, a)
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "owner@example.com") {
		t.Fatalf("nested identity leaked: %s", encoded)
	}
	if nested["message"] != "owner@example.com" || tags[0] != "owner@example.com" || nested["count"] != 3 {
		t.Fatal("shared input containers were mutated")
	}
}
