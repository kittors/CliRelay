package oauth

import (
	"errors"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestServiceSetPatchAndDeleteExcludedModels(t *testing.T) {
	cfg := &config.Config{}
	service := NewService(cfg)

	service.SetExcludedModels(map[string][]string{
		" Codex ": {" gpt-5 ", "", "gpt-5"},
		" ":       {"ignored"},
	})
	wantAfterSet := map[string][]string{"codex": {"gpt-5"}}
	if !reflect.DeepEqual(cfg.OAuthExcludedModels, wantAfterSet) {
		t.Fatalf("excluded models after set = %#v", cfg.OAuthExcludedModels)
	}

	if _, err := service.PatchExcludedModels(" CODEX ", []string{" gpt-5.3-codex "}); err != nil {
		t.Fatalf("patch excluded models returned error: %v", err)
	}
	wantAfterPatch := map[string][]string{"codex": {"gpt-5.3-codex"}}
	if !reflect.DeepEqual(cfg.OAuthExcludedModels, wantAfterPatch) {
		t.Fatalf("excluded models after patch = %#v", cfg.OAuthExcludedModels)
	}

	if _, err := service.PatchExcludedModels("codex", nil); err != nil {
		t.Fatalf("empty patch should delete existing provider: %v", err)
	}
	if cfg.OAuthExcludedModels != nil {
		t.Fatalf("excluded models were not cleared: %#v", cfg.OAuthExcludedModels)
	}

	if _, err := service.PatchExcludedModels("codex", nil); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("missing provider patch error = %v, want ErrProviderNotFound", err)
	}
	if _, err := service.DeleteExcludedModels(" "); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("blank delete error = %v, want ErrInvalidProvider", err)
	}
}

func TestServiceSetPatchAndDeleteModelAlias(t *testing.T) {
	cfg := &config.Config{}
	service := NewService(cfg)

	service.SetModelAlias(map[string][]config.OAuthModelAlias{
		" Codex ": {
			{Name: " gpt-5.3-codex ", Alias: " codex-latest ", Fork: true},
			{Name: "same", Alias: "same"},
			{Name: "", Alias: "ignored"},
		},
	})
	wantAfterSet := map[string][]config.OAuthModelAlias{
		"codex": {{Name: "gpt-5.3-codex", Alias: "codex-latest", Fork: true}},
	}
	if !reflect.DeepEqual(cfg.OAuthModelAlias, wantAfterSet) {
		t.Fatalf("model alias after set = %#v", cfg.OAuthModelAlias)
	}

	if _, err := service.PatchModelAlias(" CODEX ", []config.OAuthModelAlias{
		{Name: "gpt-5.3-codex", Alias: "codex-main"},
		{Name: "gpt-5.3-codex", Alias: "codex-main"},
	}); err != nil {
		t.Fatalf("patch model alias returned error: %v", err)
	}
	wantAfterPatch := map[string][]config.OAuthModelAlias{
		"codex": {{Name: "gpt-5.3-codex", Alias: "codex-main"}},
	}
	if !reflect.DeepEqual(cfg.OAuthModelAlias, wantAfterPatch) {
		t.Fatalf("model alias after patch = %#v", cfg.OAuthModelAlias)
	}

	if _, err := service.PatchModelAlias("codex", nil); err != nil {
		t.Fatalf("empty patch should delete existing channel: %v", err)
	}
	if cfg.OAuthModelAlias != nil {
		t.Fatalf("model alias was not cleared: %#v", cfg.OAuthModelAlias)
	}

	if _, err := service.PatchModelAlias("codex", nil); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("missing channel patch error = %v, want ErrChannelNotFound", err)
	}
	if _, err := service.DeleteModelAlias(" "); !errors.Is(err, ErrInvalidChannel) {
		t.Fatalf("blank delete error = %v, want ErrInvalidChannel", err)
	}
}

func TestNormalizeModelAliasDoesNotMutateInput(t *testing.T) {
	input := map[string][]config.OAuthModelAlias{
		" Codex ": {{Name: " gpt-5.3-codex ", Alias: " codex-latest "}},
	}

	normalized := NormalizeModelAlias(input)
	if _, ok := normalized["codex"]; !ok {
		t.Fatalf("normalized aliases = %#v", normalized)
	}
	if input[" Codex "][0].Name != " gpt-5.3-codex " {
		t.Fatalf("input was mutated: %#v", input)
	}
}
