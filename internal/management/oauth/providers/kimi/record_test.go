package kimi

import (
	"testing"
	"time"

	internalkimi "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
)

func TestRecordFromAuthBundleBuildsPersistableRecord(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 30, 0, 123000000, time.UTC)
	expiresAt := now.Add(time.Hour).Unix()
	storage := &internalkimi.KimiTokenStorage{AccessToken: "storage-access"}
	bundle := &internalkimi.KimiAuthBundle{
		TokenData: &internalkimi.KimiTokenData{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			TokenType:    "Bearer",
			Scope:        "openid profile",
			ExpiresAt:    expiresAt,
		},
		DeviceID: " device-1 ",
	}

	record := RecordFromAuthBundle(storage, bundle, now)
	if record == nil {
		t.Fatal("RecordFromAuthBundle() = nil")
	}
	if record.ID != "kimi-1780749000123.json" || record.FileName != "kimi-1780749000123.json" {
		t.Fatalf("ID/FileName = %q/%q, want timestamped kimi filename", record.ID, record.FileName)
	}
	if record.Provider != "kimi" || record.Label != "Kimi User" || record.Storage != storage {
		t.Fatalf("provider/label/storage = %q/%q/%#v", record.Provider, record.Label, record.Storage)
	}

	for key, want := range map[string]string{
		"type":          "kimi",
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"scope":         "openid profile",
		"expired":       time.Unix(expiresAt, 0).UTC().Format(time.RFC3339),
		"device_id":     "device-1",
	} {
		if got, _ := record.Metadata[key].(string); got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
	}
	if got, _ := record.Metadata["timestamp"].(int64); got != now.UnixMilli() {
		t.Fatalf("metadata[timestamp] = %#v, want %d", record.Metadata["timestamp"], now.UnixMilli())
	}
}

func TestMetadataFromAuthBundleOmitsOptionalFields(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 30, 0, 0, time.UTC)
	metadata := MetadataFromAuthBundle(&internalkimi.KimiAuthBundle{
		TokenData: &internalkimi.KimiTokenData{},
		DeviceID:  " ",
	}, now)

	if _, ok := metadata["expired"]; ok {
		t.Fatalf("metadata[expired] = %#v, want omitted", metadata["expired"])
	}
	if _, ok := metadata["device_id"]; ok {
		t.Fatalf("metadata[device_id] = %#v, want omitted", metadata["device_id"])
	}
}

func TestRecordFromAuthBundleHandlesNilInputs(t *testing.T) {
	if record := RecordFromAuthBundle(nil, &internalkimi.KimiAuthBundle{}, time.Time{}); record != nil {
		t.Fatalf("RecordFromAuthBundle(nil storage) = %#v, want nil", record)
	}
	if record := RecordFromAuthBundle(&internalkimi.KimiTokenStorage{}, nil, time.Time{}); record != nil {
		t.Fatalf("RecordFromAuthBundle(nil bundle) = %#v, want nil", record)
	}
	if record := RecordFromAuthBundle(&internalkimi.KimiTokenStorage{}, &internalkimi.KimiAuthBundle{}, time.Time{}); record != nil {
		t.Fatalf("RecordFromAuthBundle(nil token data) = %#v, want nil", record)
	}
}
