package amp

import (
	"errors"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestServiceScalarFields(t *testing.T) {
	cfg := &config.Config{}
	service := NewService(cfg)

	service.SetUpstreamURL(" https://amp.example.test/api ")
	service.SetUpstreamAPIKey(" upstream-key ")
	service.SetRestrictManagementToLocalhost(true)
	service.SetForceModelMappings(true)

	if got := cfg.AmpCode.UpstreamURL; got != "https://amp.example.test/api" {
		t.Fatalf("upstream url = %q", got)
	}
	if got := cfg.AmpCode.UpstreamAPIKey; got != "upstream-key" {
		t.Fatalf("upstream api key = %q", got)
	}
	if !cfg.AmpCode.RestrictManagementToLocalhost {
		t.Fatal("restrict management flag was not updated")
	}
	if !cfg.AmpCode.ForceModelMappings {
		t.Fatal("force model mappings flag was not updated")
	}

	service.ClearUpstreamURL()
	service.ClearUpstreamAPIKey()

	if cfg.AmpCode.UpstreamURL != "" {
		t.Fatalf("upstream url was not cleared: %q", cfg.AmpCode.UpstreamURL)
	}
	if cfg.AmpCode.UpstreamAPIKey != "" {
		t.Fatalf("upstream api key was not cleared: %q", cfg.AmpCode.UpstreamAPIKey)
	}
}

func TestServicePatchAndDeleteModelMappings(t *testing.T) {
	cfg := &config.Config{
		AmpCode: config.AmpCode{
			ModelMappings: []config.AmpModelMapping{
				{From: " claude-opus ", To: "old-target"},
				{From: "claude-haiku", To: "keep-target"},
			},
		},
	}
	service := NewService(cfg)

	service.PatchModelMappings([]config.AmpModelMapping{
		{From: "claude-opus", To: "new-target"},
		{From: "claude-sonnet", To: "sonnet-target", Regex: true},
	})

	wantAfterPatch := []config.AmpModelMapping{
		{From: "claude-opus", To: "new-target"},
		{From: "claude-haiku", To: "keep-target"},
		{From: "claude-sonnet", To: "sonnet-target", Regex: true},
	}
	if !reflect.DeepEqual(cfg.AmpCode.ModelMappings, wantAfterPatch) {
		t.Fatalf("model mappings after patch = %#v", cfg.AmpCode.ModelMappings)
	}

	service.DeleteModelMappings([]string{" claude-haiku "})
	wantAfterDelete := []config.AmpModelMapping{
		{From: "claude-opus", To: "new-target"},
		{From: "claude-sonnet", To: "sonnet-target", Regex: true},
	}
	if !reflect.DeepEqual(cfg.AmpCode.ModelMappings, wantAfterDelete) {
		t.Fatalf("model mappings after delete = %#v", cfg.AmpCode.ModelMappings)
	}

	service.DeleteModelMappings(nil)
	if cfg.AmpCode.ModelMappings != nil {
		t.Fatalf("model mappings were not cleared: %#v", cfg.AmpCode.ModelMappings)
	}
}

func TestServiceNormalizePatchAndDeleteUpstreamAPIKeys(t *testing.T) {
	cfg := &config.Config{}
	service := NewService(cfg)

	service.SetUpstreamAPIKeys([]config.AmpUpstreamAPIKeyEntry{
		{UpstreamAPIKey: " upstream-a ", APIKeys: []string{" client-a ", "", "client-b"}},
		{UpstreamAPIKey: " ", APIKeys: []string{"ignored"}},
	})

	wantAfterSet := []config.AmpUpstreamAPIKeyEntry{
		{UpstreamAPIKey: "upstream-a", APIKeys: []string{"client-a", "client-b"}},
	}
	if !reflect.DeepEqual(cfg.AmpCode.UpstreamAPIKeys, wantAfterSet) {
		t.Fatalf("upstream api keys after set = %#v", cfg.AmpCode.UpstreamAPIKeys)
	}

	service.PatchUpstreamAPIKeys([]config.AmpUpstreamAPIKeyEntry{
		{UpstreamAPIKey: " upstream-a ", APIKeys: []string{" client-c "}},
		{UpstreamAPIKey: "upstream-b", APIKeys: []string{"client-d", " "}},
		{UpstreamAPIKey: " ", APIKeys: []string{"ignored"}},
	})

	wantAfterPatch := []config.AmpUpstreamAPIKeyEntry{
		{UpstreamAPIKey: "upstream-a", APIKeys: []string{"client-c"}},
		{UpstreamAPIKey: "upstream-b", APIKeys: []string{"client-d"}},
	}
	if !reflect.DeepEqual(cfg.AmpCode.UpstreamAPIKeys, wantAfterPatch) {
		t.Fatalf("upstream api keys after patch = %#v", cfg.AmpCode.UpstreamAPIKeys)
	}

	if err := service.DeleteUpstreamAPIKeys([]string{" upstream-a "}); err != nil {
		t.Fatalf("delete upstream api keys returned error: %v", err)
	}
	wantAfterDelete := []config.AmpUpstreamAPIKeyEntry{
		{UpstreamAPIKey: "upstream-b", APIKeys: []string{"client-d"}},
	}
	if !reflect.DeepEqual(cfg.AmpCode.UpstreamAPIKeys, wantAfterDelete) {
		t.Fatalf("upstream api keys after delete = %#v", cfg.AmpCode.UpstreamAPIKeys)
	}

	if err := service.DeleteUpstreamAPIKeys([]string{" "}); !errors.Is(err, ErrEmptyValue) {
		t.Fatalf("delete empty values error = %v, want ErrEmptyValue", err)
	}

	if err := service.DeleteUpstreamAPIKeys(nil); err != nil {
		t.Fatalf("clear upstream api keys returned error: %v", err)
	}
	if cfg.AmpCode.UpstreamAPIKeys != nil {
		t.Fatalf("upstream api keys were not cleared: %#v", cfg.AmpCode.UpstreamAPIKeys)
	}
}
