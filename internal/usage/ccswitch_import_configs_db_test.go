package usage

import "testing"

func TestReplaceAllCcSwitchImportConfigsPersistsModelMappings(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()
	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}
	initCcSwitchImportConfigsTable(db)

	err := ReplaceAllCcSwitchImportConfigs([]CcSwitchImportConfigRow{
		{
			ID:                   "cfg-claude",
			ClientType:           "claude",
			ProviderName:         "Relay Claude",
			DefaultModel:         "kimi-k2.5",
			AllowedChannelGroups: []string{"kimicode"},
			EndpointPath:         "/v1",
			UsageAutoInterval:    30,
			APIKeyField:          "ANTHROPIC_API_KEY",
			ModelMappings: []CcSwitchModelMappingRow{
				{Role: "main", RequestModel: "kimi-k2.5", TargetModel: "kimi-k2.5"},
				{Role: "haiku", RequestModel: "claude-3-5-haiku", TargetModel: "kimi-k2.5"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs() error = %v", err)
	}

	rows := ListCcSwitchImportConfigs()
	if len(rows) != 1 {
		t.Fatalf("ListCcSwitchImportConfigs() length = %d, want 1: %#v", len(rows), rows)
	}
	if len(rows[0].ModelMappings) != 2 {
		t.Fatalf("model mappings length = %d, want 2: %#v", len(rows[0].ModelMappings), rows[0].ModelMappings)
	}
	if rows[0].ModelMappings[1].Role != "haiku" ||
		rows[0].ModelMappings[1].RequestModel != "claude-3-5-haiku" ||
		rows[0].ModelMappings[1].TargetModel != "kimi-k2.5" {
		t.Fatalf("model mapping not preserved: %#v", rows[0].ModelMappings[1])
	}
}

func TestReplaceAllCcSwitchImportConfigsRejectsDuplicateRoutePaths(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()
	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}
	initCcSwitchImportConfigsTable(db)

	err := ReplaceAllCcSwitchImportConfigs([]CcSwitchImportConfigRow{
		{
			ID:                   "cfg-kimi-a",
			ClientType:           "claude",
			ProviderName:         "Kimi A",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"kimicode"},
			RoutePath:            "/kimicode/cs_same",
			EndpointPath:         "",
			UsageAutoInterval:    30,
		},
		{
			ID:                   "cfg-kimi-b",
			ClientType:           "claude",
			ProviderName:         "Kimi B",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"kimicode"},
			RoutePath:            "kimicode/cs_same/",
			EndpointPath:         "",
			UsageAutoInterval:    30,
		},
	})
	if err == nil {
		t.Fatal("ReplaceAllCcSwitchImportConfigs() error = nil, want duplicate route-path error")
	}
}

func TestFindCcSwitchImportConfigByRoutePath(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()
	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}
	initCcSwitchImportConfigsTable(db)

	if err := ReplaceAllCcSwitchImportConfigs([]CcSwitchImportConfigRow{
		{
			ID:                   "cfg-kimi-route",
			ClientType:           "claude",
			ProviderName:         "Kimi route",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"kimicode"},
			RoutePath:            "/kimicode/cs_abcd1234",
			EndpointPath:         "",
			UsageAutoInterval:    30,
			ModelMappings: []CcSwitchModelMappingRow{
				{Role: "opus", RequestModel: "claude-opus-4-7", TargetModel: "kimi-k2.6"},
			},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs() error = %v", err)
	}

	row, ok := FindCcSwitchImportConfigByRoutePath("https://relay.example.com/kimicode/cs_abcd1234/")
	if !ok {
		t.Fatal("FindCcSwitchImportConfigByRoutePath() ok = false, want true")
	}
	if row.ID != "cfg-kimi-route" {
		t.Fatalf("row.ID = %q, want %q", row.ID, "cfg-kimi-route")
	}
	if row.RoutePath != "/kimicode/cs_abcd1234" {
		t.Fatalf("row.RoutePath = %q, want normalized route path", row.RoutePath)
	}
	if len(row.ModelMappings) != 1 || row.ModelMappings[0].TargetModel != "kimi-k2.6" {
		t.Fatalf("row.ModelMappings = %#v, want kimi-k2.6 mapping", row.ModelMappings)
	}
}
