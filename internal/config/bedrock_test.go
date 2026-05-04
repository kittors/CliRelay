package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadsBedrockKeys(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`port: 8317
bedrock-api-key:
  - name: bedrock bearer
    auth-mode: api-key
    api-key: br-key
    region: eu-west-1
    force-global: true
    base-url: https://bedrock.local
    prefix: aws
    proxy-url: http://proxy.local:8080
    proxy-id: hk
    headers:
      X-Test: yes
    models:
      - name: claude-sonnet-4-5
        alias: aws-sonnet
    excluded-models:
      - claude-opus-*
  - name: bedrock sigv4
    auth-mode: sigv4
    access-key-id: AKIATEST
    secret-access-key: SECRET
    session-token: SESSION
    region: us-east-1
`)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if len(cfg.BedrockKey) != 2 {
		t.Fatalf("expected 2 bedrock keys, got %d", len(cfg.BedrockKey))
	}
	first := cfg.BedrockKey[0]
	if first.AuthMode != "api-key" {
		t.Fatalf("first auth-mode = %q, want api-key", first.AuthMode)
	}
	if first.APIKey != "br-key" || first.Region != "eu-west-1" || !first.ForceGlobal {
		t.Fatalf("unexpected first credential: %+v", first)
	}
	if first.Headers["X-Test"] != "yes" {
		t.Fatalf("expected header to survive normalization, got %+v", first.Headers)
	}
	if len(first.Models) != 1 || first.Models[0].Alias != "aws-sonnet" {
		t.Fatalf("unexpected models: %+v", first.Models)
	}
	second := cfg.BedrockKey[1]
	if second.AuthMode != "sigv4" {
		t.Fatalf("second auth-mode = %q, want sigv4", second.AuthMode)
	}
	if second.AccessKeyID != "AKIATEST" || second.SecretAccessKey != "SECRET" || second.SessionToken != "SESSION" {
		t.Fatalf("unexpected sigv4 credential: %+v", second)
	}
}

func TestSanitizeBedrockKeys(t *testing.T) {
	cfg := &Config{
		BedrockKey: []BedrockKey{
			{
				Name:            " sig ",
				AuthMode:        " ",
				AccessKeyID:     " AKIA ",
				SecretAccessKey: " SECRET ",
				Region:          " ",
				Prefix:          "/team/",
				Headers:         map[string]string{" x-test ": " ok "},
				Models: []BedrockModel{
					{Name: " claude-sonnet-4-5 ", Alias: " sonnet "},
					{Name: " ", Alias: " "},
				},
				ExcludedModels: []string{" claude-opus-* ", ""},
			},
			{AuthMode: "sigv4", AccessKeyID: "AKIA", SecretAccessKey: "SECRET"},
			{AuthMode: "api-key", APIKey: " br-key ", Region: " ap-southeast-2 "},
			{AuthMode: "api-key", APIKey: " "},
			{AuthMode: "sigv4", AccessKeyID: "missing-secret"},
		},
	}

	cfg.SanitizeBedrockKeys()

	if len(cfg.BedrockKey) != 2 {
		t.Fatalf("expected 2 sanitized bedrock keys, got %d: %+v", len(cfg.BedrockKey), cfg.BedrockKey)
	}
	sig := cfg.BedrockKey[0]
	if sig.AuthMode != "sigv4" || sig.Region != "us-east-1" || sig.Prefix != "team" {
		t.Fatalf("unexpected sigv4 normalized fields: %+v", sig)
	}
	if sig.AccessKeyID != "AKIA" || sig.SecretAccessKey != "SECRET" {
		t.Fatalf("unexpected sigv4 credentials: %+v", sig)
	}
	if sig.Headers["x-test"] != "ok" {
		t.Fatalf("expected normalized header, got %+v", sig.Headers)
	}
	if len(sig.Models) != 1 || sig.Models[0].Name != "claude-sonnet-4-5" || sig.Models[0].Alias != "sonnet" {
		t.Fatalf("unexpected sanitized models: %+v", sig.Models)
	}
	if len(sig.ExcludedModels) != 1 || sig.ExcludedModels[0] != "claude-opus-*" {
		t.Fatalf("unexpected excluded models: %+v", sig.ExcludedModels)
	}
	apiKey := cfg.BedrockKey[1]
	if apiKey.AuthMode != "api-key" || apiKey.APIKey != "br-key" || apiKey.Region != "ap-southeast-2" {
		t.Fatalf("unexpected api-key normalized fields: %+v", apiKey)
	}
}
