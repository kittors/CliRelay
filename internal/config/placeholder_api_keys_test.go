package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestIsPlaceholderAPIKeyMatchesOnlyTheExampleKeys(t *testing.T) {
	for _, key := range []string{"your-api-key-1", "your-api-key-2", "your-api-key-3", " your-api-key-2\t"} {
		if !IsPlaceholderAPIKey(key) {
			t.Fatalf("IsPlaceholderAPIKey(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"", "your-api-key-4", "your-api-key-10", "Your-Api-Key-1", "your-api-key-1x", "sk-your-api-key-1", "your-api-key-"} {
		if IsPlaceholderAPIKey(key) {
			t.Fatalf("IsPlaceholderAPIKey(%q) = true, want false", key)
		}
	}
}

// The template may keep showing example keys, but only ones the server refuses:
// otherwise uncommenting the example hands out a credential that is published
// with every copy of the repository.
func TestExampleConfigShowsOnlyRejectedPlaceholderKeys(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	// List items only: those become keys once uncommented, prose around them does not.
	shown := regexp.MustCompile(`-\s+"?(your-api-key[^"\s#]*)`).FindAllStringSubmatch(string(data), -1)
	if len(shown) == 0 {
		t.Fatal("config.example.yaml no longer shows the example keys; drop this guard together with them")
	}
	for _, match := range shown {
		if !IsPlaceholderAPIKey(match[1]) {
			t.Fatalf("config.example.yaml shows %q, which IsPlaceholderAPIKey does not reject", match[1])
		}
	}
}
