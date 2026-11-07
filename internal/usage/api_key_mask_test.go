package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestMaskAPIKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"very short", "ab", "***"},
		{"four characters", "abcd", "***"},
		{"five characters", "abcde", "ab***de"},
		{"ten characters", "0123456789", "01***89"},
		{"eleven characters", "sk-thinking", "sk-thi***king"},
		{"long key", "sk-owned-log-a", "sk-own***og-a"},
		{"trimmed", "  sk-owned-log-a  ", "sk-own***og-a"},
		// Cutting a multi-byte character in half would produce text PostgreSQL
		// refuses to store, so the mask is built from runes.
		{"multi-byte", "密钥一二三四五六七八九十", "密钥一二三四***七八九十"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MaskAPIKey(tc.value)
			if got != tc.want {
				t.Fatalf("MaskAPIKey(%q) = %q, want %q", tc.value, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("MaskAPIKey(%q) = %q is not valid UTF-8", tc.value, got)
			}
			// Masking a mask must not change it: the migration and the read path
			// both rely on that to be safe to repeat.
			if again := MaskAPIKey(got); again != got {
				t.Fatalf("MaskAPIKey is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestMaskAPIKeyNeverRevealsTheMiddle(t *testing.T) {
	t.Parallel()

	// The X keeps the CI secret scan (sk- followed by 16+ alphanumerics) from
	// mistaking this fixture for a real key.
	secret := "sk-abcdefghijklmnopXrstuvwxyz0123456789"
	masked := MaskAPIKey(secret)
	if middle := secret[10:30]; strings.Contains(masked, middle) {
		t.Fatalf("mask %q keeps the middle %q of the secret", masked, middle)
	}
	if len(masked) >= len(secret) {
		t.Fatalf("mask %q is not shorter than the secret", masked)
	}
}

func TestIsMaskedAPIKey(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"":                       false,
		"***":                    true,
		"ab***de":                true,
		"sk-own***og-a":          true,
		" sk-own***og-a ":        true,
		"sk-owned-log-a":         false,
		"POST /v1/models":        false,
		"warmup":                 false,
		"sk-own***og-a-and-more": false,
		// Contains the filler but is not what MaskAPIKey would produce for itself,
		// so it is a secret that merely contains asterisks.
		"sk***x": false,
	}
	for value, want := range cases {
		if got := IsMaskedAPIKey(value); got != want {
			t.Errorf("IsMaskedAPIKey(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestIsRequestLogLabel(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"":                              false,
		"/healthz":                      true,
		"/v1/chat/completions":          true,
		"POST /v1/chat/completions":     true,
		"post /v1/chat/completions":     true,
		"GET /v1/models":                true,
		"DELETE /auth-files/x":          true,
		"PATCH /x":                      true,
		"OPTIONS /x":                    true,
		"HEAD /x":                       true,
		"warmup":                        true,
		"Warmup":                        true,
		"codex":                         true,
		"antigravity":                   true,
		"system":                        true,
		"__system__":                    true,
		"unknown":                       true,
		"sk-live-secret-0001":           false,
		"sk-own***og-a":                 false,
		"POST":                          false,
		"POST/v1":                       false,
		"GETX /a":                       false,
		"some-custom-client-key-123456": false,
	}
	for value, want := range cases {
		if got := IsRequestLogLabel(value); got != want {
			t.Errorf("IsRequestLogLabel(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestStoredRequestLogAPIKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		apiKey   string
		known    bool
		expected string
	}{
		{"empty", "", false, ""},
		{"label without key", "POST /v1/models", false, "POST /v1/models"},
		{"origin without key", "warmup", false, "warmup"},
		{"secret without key row", "sk-unregistered-secret", false, MaskAPIKey("sk-unregistered-secret")},
		{"secret with key row", "sk-registered-secret", true, MaskAPIKey("sk-registered-secret")},
		// A stored key is a secret whatever it reads like.
		{"label-shaped secret of a stored key", "warmup", true, MaskAPIKey("warmup")},
		{"mask stays", "sk-own***og-a", false, "sk-own***og-a"},
	}
	for _, tc := range cases {
		if got := storedRequestLogAPIKey(tc.apiKey, tc.known); got != tc.expected {
			t.Errorf("%s: storedRequestLogAPIKey(%q, %v) = %q, want %q", tc.name, tc.apiKey, tc.known, got, tc.expected)
		}
	}
}

func TestRequestLogAPIKeyForDisplay(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":                       "",
		"  ":                     "",
		"sk-legacy-secret-0001":  MaskAPIKey("sk-legacy-secret-0001"),
		"sk-own***og-a":          "sk-own***og-a",
		"POST /auth-files/x":     "POST /auth-files/x",
		"/healthz":               "/healthz",
		"warmup":                 "warmup",
		"  sk-legacy-secret-02 ": MaskAPIKey("sk-legacy-secret-02"),
	}
	for stored, want := range cases {
		if got := requestLogAPIKeyForDisplay(stored); got != want {
			t.Errorf("requestLogAPIKeyForDisplay(%q) = %q, want %q", stored, got, want)
		}
	}
}

func TestStatsBucketKey(t *testing.T) {
	t.Parallel()

	secret := "sk-bucket-secret-0001"
	cases := []struct {
		name, apiKey, id, want string
	}{
		{"id wins", secret, "key-1", "key-1"},
		{"id wins over a label", "warmup", "key-1", "key-1"},
		{"label without id", "POST /v1/models", "", "POST /v1/models"},
		{"empty", "", "", ""},
		{"secret without id is masked", secret, "", MaskAPIKey(secret)},
		{"blank id is no id", secret, "  ", MaskAPIKey(secret)},
	}
	for _, tc := range cases {
		if got := statsBucketKey(tc.apiKey, tc.id); got != tc.want {
			t.Errorf("%s: statsBucketKey(%q, %q) = %q, want %q", tc.name, tc.apiKey, tc.id, got, tc.want)
		}
	}
}

func TestSnapshotBucketKey(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	registerTestAPIKey(t, "bucket-key-id", "sk-snapshot-secret-0001", "Snapshot")

	cases := []struct {
		name, bucket, want string
	}{
		{"secret of a stored key becomes its id", "sk-snapshot-secret-0001", "bucket-key-id"},
		{"id of a deleted key is kept", "11111111-2222-3333-4444-555555555555", "11111111-2222-3333-4444-555555555555"},
		{"mask is kept", MaskAPIKey("sk-snapshot-secret-0001"), MaskAPIKey("sk-snapshot-secret-0001")},
		{"id of a stored key is kept", "bucket-key-id", "bucket-key-id"},
		{"label is kept", "POST /v1/models", "POST /v1/models"},
		{"origin is kept", "warmup", "warmup"},
		{"unknown secret is masked", "sk-forgotten-secret-9999", MaskAPIKey("sk-forgotten-secret-9999")},
		{"blank", "   ", ""},
	}
	for _, tc := range cases {
		if got := snapshotBucketKey(tc.bucket); got != tc.want {
			t.Errorf("%s: snapshotBucketKey(%q) = %q, want %q", tc.name, tc.bucket, got, tc.want)
		}
	}
}

func TestMergeSnapshotRekeysSecretBucketsByKeyID(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	const secretA, secretB, forgotten = "sk-merge-secret-a-0001", "sk-merge-secret-b-0002", "sk-merge-forgotten-0003"
	registerTestAPIKey(t, "merge-key-a", secretA, "A")
	registerTestAPIKey(t, "merge-key-b", secretB, "B")

	bucket := func(requests int64) APISnapshot {
		return APISnapshot{
			TotalRequests: requests,
			TotalTokens:   requests * 10,
			Models:        map[string]ModelSnapshot{"gpt-test": {TotalRequests: requests, TotalTokens: requests * 10}},
		}
	}
	stats := NewRequestStatistics()
	// What an older build exported or mirrored to Redis: buckets named after the
	// client secrets, next to buckets already named after ids and labels.
	stats.MergeSnapshot(StatisticsSnapshot{APIs: map[string]APISnapshot{
		secretA:              bucket(1),
		"merge-key-a":        bucket(2),
		secretB:              bucket(4),
		forgotten:            bucket(8),
		"POST /v1/models":    bucket(16),
		MaskAPIKey(secretB):  bucket(32),
		"warmup":             bucket(64),
		" " + secretA + "  ": bucket(128),
	}})

	snapshot := stats.Snapshot()
	want := map[string]int64{
		"merge-key-a":         1 + 2 + 128, // the secret and the id are the same key
		"merge-key-b":         4,
		MaskAPIKey(forgotten): 8,
		"POST /v1/models":     16,
		MaskAPIKey(secretB):   32,
		"warmup":              64,
	}
	if len(snapshot.APIs) != len(want) {
		t.Fatalf("snapshot buckets = %v, want %d buckets", bucketNames(snapshot), len(want))
	}
	for name, requests := range want {
		got, ok := snapshot.APIs[name]
		if !ok || got.TotalRequests != requests {
			t.Errorf("bucket %q = %+v (present=%v), want %d requests", name, got, ok, requests)
		}
	}

	// Whatever is served, exported or mirrored carries no secret.
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, secret := range []string{secretA, secretB, forgotten} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("snapshot carries the secret %q: %s", secret, encoded)
		}
	}
}

func bucketNames(snapshot StatisticsSnapshot) []string {
	names := make([]string, 0, len(snapshot.APIs))
	for name := range snapshot.APIs {
		names = append(names, name)
	}
	return names
}
