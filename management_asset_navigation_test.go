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

func TestChannelGroupsAssetExposesRoutingStrategyControl(t *testing.T) {
	_, pageContent := readManagementAssetByPrefix(t, "ChannelGroupsPage")
	for _, want := range []string{
		`"data-testid":"routing-strategy-select"`,
		`channel_groups_page.routing_strategy_tooltip`,
		`channel_groups_page.routing_strategy_round_robin`,
		`channel_groups_page.routing_strategy_fill_first`,
		`strategy:t==="fill-first"?"fill-first":"round-robin"`,
	} {
		if !strings.Contains(pageContent, want) {
			t.Fatalf("channel groups asset missing %q", want)
		}
	}

	_, zhContent := readManagementAssetByPrefix(t, "zh-CN")
	for _, want := range []string{
		`routing_strategy_tooltip:`,
		`round-robin`,
		`fill-first`,
		`prompt cache`,
	} {
		if !strings.Contains(zhContent, want) {
			t.Fatalf("zh-CN asset missing routing strategy wording %q", want)
		}
	}

	_, enContent := readManagementAssetByPrefix(t, "en")
	for _, want := range []string{
		`routing_strategy_tooltip:`,
		`round-robin`,
		`fill-first`,
		`prompt cache`,
	} {
		if !strings.Contains(enContent, want) {
			t.Fatalf("en asset missing routing strategy wording %q", want)
		}
	}
}

func TestChannelGroupsRoutingStrategyUsesSharedSelect(t *testing.T) {
	_, pageContent := readManagementAssetByPrefix(t, "ChannelGroupsPage")

	if !strings.Contains(pageContent, `from"./Select-`) {
		t.Fatalf("channel groups asset should import the shared Select component")
	}

	for _, unwanted := range []string{
		`t.jsxs("select",{"data-testid":"routing-strategy-select"`,
		`t.jsx("option",{value:"round-robin"`,
		`t.jsx("option",{value:"fill-first"`,
		`t.currentTarget.value==="fill-first"`,
	} {
		if strings.Contains(pageContent, unwanted) {
			t.Fatalf("routing strategy selector should use the shared Select component, found native select fragment %q", unwanted)
		}
	}

	_, selectContent := readManagementAssetByPrefix(t, "Select")
	for _, want := range []string{
		`disabled:G=!1,...I`,
		`disabled:G,...I`,
		`G||c(e=>!e)`,
	} {
		if !strings.Contains(selectContent, want) {
			t.Fatalf("shared Select asset should support disabled and data attribute passthrough, missing %q", want)
		}
	}
}

func TestChannelGroupsRoutingStrategyControlIsInsideGroupEditorModal(t *testing.T) {
	_, pageContent := readManagementAssetByPrefix(t, "ChannelGroupsPage")

	modalIndex := strings.Index(pageContent, `"group-editor-modal-body"`)
	if modalIndex < 0 {
		t.Fatalf("channel groups asset missing group editor modal body")
	}

	strategyIndex := strings.Index(pageContent, `"data-testid":"routing-strategy-select"`)
	if strategyIndex < 0 {
		t.Fatalf("channel groups asset missing routing strategy selector")
	}

	groupNameIndex := strings.Index(pageContent, `channel_groups_page.group_name_label`)
	if groupNameIndex < 0 {
		t.Fatalf("channel groups asset missing group name field")
	}

	if strategyIndex < modalIndex || strategyIndex > groupNameIndex {
		t.Fatalf(
			"routing strategy selector should render inside the group editor modal before the group name field: modal=%d strategy=%d groupName=%d",
			modalIndex,
			strategyIndex,
			groupNameIndex,
		)
	}
}

func TestChannelGroupsRoutingStrategyControlIsGroupScoped(t *testing.T) {
	_, pageContent := readManagementAssetByPrefix(t, "ChannelGroupsPage")

	if strings.Contains(pageContent, `value:a.routingStrategy`) ||
		strings.Contains(pageContent, `L({routingStrategy:`) {
		t.Fatalf("routing strategy selector should update the group editor form, not the page-level routing strategy")
	}

	for _, want := range []string{
		`strategy:t==="fill-first"?"fill-first":"round-robin"`,
		`value:h.strategy`,
		`strategy:h.strategy==="fill-first"?"fill-first":"round-robin"`,
		`strategy:i?.strategy==="fill-first"?"fill-first":"round-robin"`,
	} {
		if !strings.Contains(pageContent, want) {
			t.Fatalf("channel groups asset missing group-scoped routing strategy binding %q", want)
		}
	}
}

func TestChannelGroupsPageOmitsRedundantIntroCopy(t *testing.T) {
	_, pageContent := readManagementAssetByPrefix(t, "ChannelGroupsPage")
	for _, unwanted := range []string{
		`a("channel_groups_page.description")`,
		`a("channel_groups_page.editor_hint")`,
		`r("channel_groups_page.groups_table_title")`,
		`r("channel_groups_page.groups_table_desc")`,
	} {
		if strings.Contains(pageContent, unwanted) {
			t.Fatalf("channel groups page should not render redundant copy key %q", unwanted)
		}
	}

	_, zhContent := readManagementAssetByPrefix(t, "zh-CN")
	for _, unwanted := range []string{
		`在独立页面维护渠道分组和路径路由`,
		`这里只维护渠道分组和路径路由，页面已简化为必要操作。`,
		`groups_table_title:"渠道分组"`,
		`把多个渠道归到一个组，供路径入口和 API Key 权限复用。`,
	} {
		if strings.Contains(zhContent, unwanted) {
			t.Fatalf("zh-CN asset should not contain redundant channel groups copy %q", unwanted)
		}
	}

	_, enContent := readManagementAssetByPrefix(t, "en")
	for _, unwanted := range []string{
		`Manage channel groups and path routing in one dedicated page.`,
		`This page only manages channel groups and path routes, with the flow kept intentionally simple.`,
		`groups_table_title:"Channel groups"`,
		`Group multiple channels together for path entries and API key permissions.`,
		`Group actual channels, then reuse those groups in paths and API key permissions.`,
	} {
		if strings.Contains(enContent, unwanted) {
			t.Fatalf("en asset should not contain redundant channel groups copy %q", unwanted)
		}
	}
}
