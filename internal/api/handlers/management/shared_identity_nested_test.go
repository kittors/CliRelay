package management

import (
	"encoding/json"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// A shared account's restrictions quote the account in their status messages
// and quota reasons as often as its top-level status message does.
func TestSharedAccountIdentityMasksNestedRestrictions(t *testing.T) {
	a := &coreauth.Auth{ID: "claude-owner@example.com.json", Provider: "claude", Label: "owner@example.com",
		Metadata: map[string]any{"email": "owner@example.com"}, Attributes: map[string]string{"runtime_only": "true"},
		Status: coreauth.StatusError, StatusMessage: "account owner@example.com blocked",
		Quota:       coreauth.QuotaState{Exceeded: true, Reason: "owner@example.com quota exceeded"},
		ModelStates: map[string]*coreauth.ModelState{"model": {Status: coreauth.StatusError, LastError: &coreauth.Error{Message: "owner@example.com model blocked"}}}}
	entries := sharedAuthFileEntries([]*coreauth.Auth{a})
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "owner@example.com") {
		t.Fatalf("the shared account's identity leaked: %s", encoded)
	}
	restrictions, ok := entries[0]["restrictions"].([]map[string]any)
	if !ok || len(restrictions) != 2 {
		t.Fatalf("restrictions lost: %v", entries[0]["restrictions"])
	}
	name := a.SharedName()
	if restrictions[0]["status_message"] != "account "+name+" blocked" || restrictions[0]["reason"] != name+" quota exceeded" {
		t.Fatalf("restriction = %v", restrictions[0])
	}
	if a.StatusMessage != "account owner@example.com blocked" || a.ModelStates["model"].LastError.Message != "owner@example.com model blocked" {
		t.Fatal("the owner's account was changed")
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
		t.Fatalf("a nested identity leaked: %s", encoded)
	}
	if nested["message"] != "owner@example.com" || tags[0] != "owner@example.com" || nested["count"] != 3 {
		t.Fatal("containers the entry shares with the owner were changed in place")
	}
}
