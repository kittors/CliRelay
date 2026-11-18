package authfiles

import (
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBuildTagPayloadCombinesDefaultAndCustomTags(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"plan_type":   "pro",
			"custom_tags": []any{" Team A ", "pro"},
		},
	}

	got := BuildTagPayload(auth)
	if !reflect.DeepEqual(got.DefaultTags, []string{"codex", "pro"}) {
		t.Fatalf("DefaultTags = %#v, want [codex pro]", got.DefaultTags)
	}
	if !reflect.DeepEqual(got.CustomTags, []string{"team-a", "pro"}) {
		t.Fatalf("CustomTags = %#v, want [team-a pro]", got.CustomTags)
	}
	if !reflect.DeepEqual(got.DisplayTags, []string{"codex", "pro", "team-a"}) {
		t.Fatalf("DisplayTags = %#v, want [codex pro team-a]", got.DisplayTags)
	}
}

func TestBuildTagPayloadHonorsHiddenAndExplicitDisplayTags(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"plan_type":           "free",
			"custom_tags":         []string{"vip"},
			"hidden_default_tags": []string{"codex"},
			"display_tags":        []string{"codex", "plus"},
		},
	}

	got := BuildTagPayload(auth)
	if !reflect.DeepEqual(got.HiddenDefaultTags, []string{"codex"}) {
		t.Fatalf("HiddenDefaultTags = %#v, want [codex]", got.HiddenDefaultTags)
	}
	if !reflect.DeepEqual(got.DisplayTags, []string{"codex", "free"}) {
		t.Fatalf("DisplayTags = %#v, want [codex free]", got.DisplayTags)
	}
}

func TestNormalizeEditableTagsRejectsTooManyCustomTags(t *testing.T) {
	if _, err := NormalizeEditableTags([]string{"one", "two", "three", "four"}, MaxCustomTags); err == nil {
		t.Fatal("expected max custom tag error")
	}
}
