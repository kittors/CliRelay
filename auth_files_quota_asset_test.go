package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readActiveAuthFilesAsset(t *testing.T) (string, string) {
	t.Helper()

	return readActivePageChunkFromIndex(t, "AuthFilesPage")
}

func TestAuthFilesQuotaAssetSupportsAnthropicOAuthUsage(t *testing.T) {
	_, content := readActiveAuthFilesAsset(t)

	for _, want := range []string{
		`https://api.anthropic.com/api/oauth/usage`,
		`oauth-2025-04-20`,
		`five_hour`,
		`seven_day`,
		`seven_day_sonnet`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("auth files quota asset missing Anthropic OAuth usage support marker %q", want)
		}
	}
}

func TestAuthFilesQuotaColumnHasInlineRefreshAction(t *testing.T) {
	_, content := readActiveAuthFilesAsset(t)
	quotaIdx := strings.Index(content, `key:"quota"`)
	if quotaIdx < 0 {
		t.Fatal("auth files asset missing quota column")
	}
	enabledIdx := strings.Index(content[quotaIdx:], `key:"enabled"`)
	if enabledIdx < 0 {
		t.Fatal("auth files asset missing enabled column after quota column")
	}
	quotaColumn := content[quotaIdx : quotaIdx+enabledIdx]
	if !strings.Contains(quotaColumn, `auth_files.col_quota`) || !strings.Contains(content, `common.refresh`) {
		t.Fatal("quota column should expose an inline refresh action")
	}
}

func TestAuthFilesQuotaAssetSupportsCurrentAntigravityModelCatalog(t *testing.T) {
	_, content := readActiveAuthFilesAsset(t)

	for _, want := range []string{
		`agentModelSorts`,
		`commandModelIds`,
		`imageGenerationModelIds`,
		`tabModelIds`,
		`defaultAgentModelId`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("auth files quota asset missing current Antigravity catalog marker %q", want)
		}
	}
}

func TestAuthFilesQuotaAssetShowsAntigravityModelMetrics(t *testing.T) {
	_, content := readActiveAuthFilesAsset(t)

	for _, want := range []string{
		`agentModelSorts`,
		`defaultAgentModelId`,
		`commandModelIds`,
		`imageGenerationModelIds`,
		`suppressItemMeta`,
		`grid-cols-[minmax(0,1fr)_0.875rem_auto_3.25rem]`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("auth files quota asset missing Antigravity model metrics marker %q", want)
		}
	}
}

func TestAuthFilesQuotaAssetDoesNotFallBackToStaticAntigravityBuckets(t *testing.T) {
	_, content := readActiveAuthFilesAsset(t)

	for _, stale := range []string{
		`Sa=[{id:"claude-gpt"`,
		`label:"Claude/GPT"`,
		`label:"Gemini 3 Pro"`,
		`Sa.forEach`,
	} {
		if strings.Contains(content, stale) {
			t.Fatalf("auth files quota asset still uses static Antigravity quota bucket %q", stale)
		}
	}
	for _, want := range []string{
		`defaultAgentModelId`,
		`agentModelSorts`,
		`commandModelIds`,
		`tabModelIds`,
		`imageGenerationModelIds`,
		`mqueryModelIds`,
		`webSearchModelIds`,
		`commitMessageModelIds`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("auth files quota asset missing dynamic Antigravity catalog marker %q", want)
		}
	}
}

func TestManagementIndexReferencesFreshAuthFilesQuotaAsset(t *testing.T) {
	name, content := readActiveAuthFilesAsset(t)
	if name == "AuthFilesPage-8ofG866A.js" {
		t.Fatalf("main asset still references previously cached auth files chunk %s", name)
	}

	for _, stale := range []string{
		`Sa=[{id:"claude-gpt"`,
		`label:"Claude/GPT"`,
		`label:"Gemini 3 Pro"`,
		`Sa.forEach`,
	} {
		if strings.Contains(content, stale) {
			t.Fatalf("fresh auth files asset still embeds stale Antigravity quota card logic %q", stale)
		}
	}
}

func TestManagementEntryAssetsBustCachedAuthFilesBundle(t *testing.T) {
	manageName, manageContent := readManagementAssetReferencedByHTML(t, "manage.html", "manage")
	indexName, _ := readManagementAssetReferencedByHTML(t, "manage.html", "index")

	for _, htmlPath := range []string{"manage.html", "management.html"} {
		content := readManagementHtml(t, htmlPath)
		for _, want := range []string{
			`/manage/assets/` + manageName + managementAssetCacheBust,
			`/manage/assets/` + indexName + managementAssetCacheBust,
		} {
			if !strings.Contains(content, want) {
				t.Fatalf("%s missing cache-busted management asset reference %q", htmlPath, want)
			}
		}
	}

	if !strings.Contains(manageContent, `./`+indexName+managementAssetCacheBust) {
		t.Fatalf("manage asset should import cache-busted index asset")
	}

	_, authContent := readActiveAuthFilesAsset(t)
	if !strings.Contains(authContent, `./`+indexName+managementAssetCacheBust) {
		t.Fatalf("auth files asset should import the same cache-busted index asset")
	}
}

func TestManagementAssetsDoNotMixBareAndCacheBustedAppModules(t *testing.T) {
	indexAsset, _ := readManagementAssetReferencedByHTML(t, "manage.html", "index")

	entries, err := os.ReadDir("assets")
	if err != nil {
		t.Fatalf("read assets dir: %v", err)
	}

	staticImport := regexp.MustCompile(`(?:from|import)\s*"(\./[^"]+\.js(?:\?[^"]*)?)"`)
	dynamicImport := regexp.MustCompile(`import\("(\./[^"]+\.js(?:\?[^"]*)?)"\)`)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		path := filepath.Join("assets", entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read asset %s: %v", path, err)
		}
		content := string(data)
		for _, match := range staticImport.FindAllStringSubmatch(content, -1) {
			ref := match[1]
			if containsAnyVendorAsset(ref) {
				continue
			}
			if !strings.HasSuffix(ref, managementAssetCacheBust) {
				t.Fatalf("%s imports app module without management cache bust: %s", path, ref)
			}
		}
		for _, match := range dynamicImport.FindAllStringSubmatch(content, -1) {
			ref := match[1]
			if containsAnyVendorAsset(ref) {
				continue
			}
			if !strings.HasSuffix(ref, managementAssetCacheBust) {
				t.Fatalf("%s dynamically imports app module without management cache bust: %s", path, ref)
			}
		}
		if entry.Name() != indexAsset && strings.Contains(content, `./`+indexAsset+`"`) {
			t.Fatalf("%s still imports bare %s, which can split shared React contexts", path, indexAsset)
		}
	}
}
