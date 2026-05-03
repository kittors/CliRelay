package main

import (
	"strings"
	"testing"
)

func TestConfigAssetUsesManageRouteForAPIKeyLink(t *testing.T) {
	_, content := readManagementAssetByPrefix(t, "ConfigPage")

	if strings.Contains(content, `href:"#/api-keys"`) {
		t.Fatalf("config asset still uses a hash API key link that BrowserRouter ignores")
	}
	if !strings.Contains(content, `href:"/manage/api-keys"`) {
		t.Fatalf("config asset missing /manage/api-keys link")
	}
}

func TestManagementAssetsExposeTotalCostSummaries(t *testing.T) {
	_, usageContent := readManagementAssetByPrefix(t, "usage")
	if !strings.Contains(usageContent, `total_cost:`) ||
		!strings.Contains(usageContent, `?.stats?.total_cost??0`) {
		t.Fatalf("usage service asset does not preserve request-log total_cost stats")
	}

	_, dashboardContent := readManagementAssetByPrefix(t, "DashboardPage")
	if !strings.Contains(dashboardContent, `?.total_cost??0`) {
		t.Fatalf("dashboard asset does not render kpi.total_cost")
	}

	_, requestLogsContent := readManagementAssetByPrefix(t, "RequestLogsPage")
	if !strings.Contains(requestLogsContent, `total_cost.toFixed(4`) {
		t.Fatalf("request logs stats strip does not render total_cost")
	}
}
