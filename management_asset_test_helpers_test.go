package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const managementAssetCacheBust = "?v=issue77-management-context"

func readManagementHtml(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readManagementAssetReferencedByHTML(t *testing.T, htmlPath, prefix string) (string, string) {
	t.Helper()

	html := readManagementHtml(t, htmlPath)
	pattern := `/manage/assets/(` + regexp.QuoteMeta(prefix) + `-[A-Za-z0-9_-]+\.js)` + regexp.QuoteMeta(managementAssetCacheBust)
	matches := regexp.MustCompile(pattern).FindAllStringSubmatch(html, -1)
	seen := make(map[string]bool)
	var names []string
	for _, match := range matches {
		name := match[1]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) != 1 {
		t.Fatalf("%s should reference exactly one %s asset, got %v", htmlPath, prefix, names)
	}
	return names[0], readManagementAsset(t, names[0])
}

func readManagementAssetByPrefix(t *testing.T, prefix string) (string, string) {
	t.Helper()

	entries, err := os.ReadDir("assets")
	if err != nil {
		t.Fatalf("read assets dir: %v", err)
	}
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-[A-Za-z0-9_-]+\.js$`)
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && pattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	if len(names) != 1 {
		t.Fatalf("assets dir should contain exactly one %s chunk, got %v", prefix, names)
	}
	return names[0], readManagementAsset(t, names[0])
}

func readActivePageChunkFromIndex(t *testing.T, prefix string) (string, string) {
	t.Helper()

	_, indexData := readManagementAssetReferencedByHTML(t, "manage.html", "index")
	pattern := regexp.MustCompile(regexp.QuoteMeta(prefix) + `-[A-Za-z0-9_-]+\.js`)
	matches := pattern.FindAllString(indexData, -1)
	seen := make(map[string]bool)
	var names []string
	for _, match := range matches {
		if !seen[match] {
			seen[match] = true
			names = append(names, match)
		}
	}
	if len(names) != 1 {
		t.Fatalf("index asset should reference exactly one %s chunk, got %v", prefix, names)
	}
	return names[0], readManagementAsset(t, names[0])
}

func readManagementAsset(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("assets", name))
	if err != nil {
		t.Fatalf("read management asset %s: %v", name, err)
	}
	return string(data)
}

func containsAnyVendorAsset(ref string) bool {
	return strings.Contains(ref, "/vendor-") || strings.HasPrefix(filepath.Base(ref), "vendor-")
}
