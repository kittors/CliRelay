package main

import (
	"os"
	"strings"
	"testing"
)

func TestRepositoryDoesNotVendorManagementPanelBuildOutput(t *testing.T) {
	for _, path := range []string{
		"assets",
		"manage.html",
		"management.html",
		"panel-meta.json",
	} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("backend repository must not vendor frontend panel build output: %s", path)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
	}
}

func TestGeneratedPanelAssetPathsAreIgnoredByGitAndDocker(t *testing.T) {
	for _, file := range []string{".gitignore", ".dockerignore"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		content := string(data)
		for _, want := range []string{
			"assets/",
			"manage.html",
			"management.html",
			"panel-meta.json",
			"dist/",
			"panel-dist.zip",
		} {
			if !strings.Contains(content, want) {
				t.Fatalf("%s missing generated panel asset ignore pattern %q", file, want)
			}
		}
	}
}
