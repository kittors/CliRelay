package main

import (
	"strings"
	"testing"
)

func TestAPIKeysPageAssetExposesSpendingLimit(t *testing.T) {
	_, content := readActivePageChunkFromIndex(t, "ApiKeysPage")

	for _, want := range []string{
		`"spending-limit"`,
		`api_keys_page.col_spending_limit`,
		`api_keys_page.spending_limit_help`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("API keys page asset missing %s", want)
		}
	}
}

func TestAPIKeysPageAssetProvidesClipboardFallback(t *testing.T) {
	_, content := readActivePageChunkFromIndex(t, "ApiKeysPage")

	if !strings.Contains(content, `navigator.clipboard`) {
		t.Fatal("API keys page asset should use navigator.clipboard when available")
	}
	if !strings.Contains(content, `.execCommand("copy")`) {
		t.Fatal("API keys page asset should fall back to document.execCommand(\"copy\")")
	}
}

func TestAPIKeysTranslationsIncludeSpendingLimit(t *testing.T) {
	for _, prefix := range []string{
		"zh-CN",
		"en",
	} {
		path, content := readManagementAssetByPrefix(t, prefix)
		for _, want := range []string{
			"col_spending_limit",
			"spending_limit_help",
		} {
			if !strings.Contains(content, want) {
				t.Fatalf("%s missing translation key %s", path, want)
			}
		}
	}
}
