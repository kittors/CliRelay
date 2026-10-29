package usage

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestBuildWhereClauseUsesParameterizedFilters(t *testing.T) {
	// A key selector is a secret the caller holds; it is resolved to the key's id
	// and the secret never reaches the SQL.
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	if err := UpsertAPIKey(APIKeyRow{ID: "key-a", Key: "sk-a", Name: "A"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-a): %v", err)
	}
	if err := UpsertAPIKey(APIKeyRow{ID: "key-b", Key: "sk-b", Name: "B"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-b): %v", err)
	}

	where, args := buildWhereClause(LogQueryParams{
		Days:                  3,
		APIKeys:               []string{" sk-a ", "sk-b", "SK-B"},
		Models:                []string{" gpt-5 ", "gpt-4"},
		Statuses:              []string{"failed"},
		AuthSubjectIDs:        []string{" authsub_29b975703f03bde1 ", ""},
		AuthIndexes:           []string{" auth-1 ", ""},
		AuthIndexChannelNames: map[string][]string{" auth-2 ": {" Legacy ", ""}},
		ChannelNames:          []string{" Codex ", ""},
	})

	wantWhere := " WHERE tenant_id = ? AND timestamp >= ? AND (api_key_id = ? OR api_key_id = ?) AND model IN (?,?) AND failed = 1 AND (auth_subject_id IN (?) OR auth_index IN (?) OR (auth_index = ? AND lower(trim(channel_name)) IN (?)) OR lower(trim(channel_name)) IN (?))"
	if where != wantWhere {
		t.Fatalf("where = %q, want %q", where, wantWhere)
	}
	if len(args) == 0 {
		t.Fatal("expected cutoff argument")
	}
	if args[0] != systemTenantID {
		t.Fatalf("tenant arg = %#v, want %q", args[0], systemTenantID)
	}
	cutoff, ok := args[1].(string)
	if !ok {
		t.Fatalf("cutoff arg type = %T, want string", args[1])
	}
	if _, err := time.Parse(time.RFC3339, cutoff); err != nil {
		t.Fatalf("cutoff arg %q is not RFC3339: %v", cutoff, err)
	}

	wantArgs := []interface{}{"key-a", "key-b", "gpt-5", "gpt-4", "authsub_29b975703f03bde1", "auth-1", "auth-2", "legacy", "codex"}
	if !reflect.DeepEqual(args[2:], wantArgs) {
		t.Fatalf("args[2:] = %#v, want %#v", args[2:], wantArgs)
	}
}

func TestBuildWhereClauseSupportsSystemRequestFilter(t *testing.T) {
	where, args := buildWhereClause(LogQueryParams{
		Days:    1,
		APIKeys: []string{SystemRequestLogFilterValue},
	})

	for _, want := range []string{
		"trim(coalesce(api_key_name, '')) = ''",
		"trim(coalesce(api_key, '')) LIKE '/%'",
		"upper(trim(coalesce(api_key, ''))) LIKE 'POST /%'",
	} {
		if !strings.Contains(where, want) {
			t.Fatalf("system filter where missing %q in %q", want, where)
		}
	}
	if len(args) != 2 {
		t.Fatalf("system filter args = %#v, want tenant and cutoff", args)
	}
}

func TestBuildWhereClauseExplicitEmptyFiltersMatchNothing(t *testing.T) {
	for _, params := range []LogQueryParams{
		{MatchNoAPIKeys: true},
		{MatchNoModels: true},
		{MatchNoStatuses: true},
		{MatchNoChannels: true},
	} {
		where, args := buildWhereClause(params)
		if where != " WHERE 1 = 0" {
			t.Fatalf("where = %q, want match-none clause for %+v", where, params)
		}
		if args != nil {
			t.Fatalf("args = %#v, want nil for %+v", args, params)
		}
	}
}

func TestBuildSingleAPIKeySelectorClauseUsesStableIdentityWhenAvailable(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	if err := UpsertAPIKey(APIKeyRow{ID: "stable-key-1", Key: "sk-live", Name: "Primary"}); err != nil {
		t.Fatalf("UpsertAPIKey() error = %v", err)
	}

	clause, args := buildSingleAPIKeySelectorClause(" sk-live ")
	wantClause := " WHERE api_key_id = ?"
	if clause != wantClause {
		t.Fatalf("clause = %q, want %q", clause, wantClause)
	}
	wantArgs := []interface{}{"stable-key-1"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}

	// A secret that is not a stored key cannot be matched against the api_key
	// column (it holds a mask), so it must select nothing rather than everything.
	clause, args = buildSingleAPIKeySelectorClause(" missing-key ")
	if clause != " WHERE 1 = 0" {
		t.Fatalf("missing key clause = %q, want a match-nothing selector", clause)
	}
	if args != nil {
		t.Fatalf("missing key args = %#v, want none", args)
	}

	// A route or internal origin is stored verbatim and still selects its rows.
	clause, args = buildSingleAPIKeySelectorClause(" POST /auth-files/warmup ")
	if clause != " WHERE api_key = ?" {
		t.Fatalf("label clause = %q, want a verbatim api_key selector", clause)
	}
	wantArgs = []interface{}{"POST /auth-files/warmup"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("label args = %#v, want %#v", args, wantArgs)
	}
}

func TestBuildWhereClauseKeyIDFilter(t *testing.T) {
	where, args := buildWhereClause(LogQueryParams{
		Days:      1,
		APIKeyIDs: []string{" key-a ", "", "key-b"},
	})
	if !strings.Contains(where, "api_key_id IN (?,?)") {
		t.Fatalf("where = %q, want an api_key_id IN filter", where)
	}
	if strings.Contains(where, "api_key =") || strings.Contains(where, "api_key IN") {
		t.Fatalf("where = %q, the key filter must not touch the api_key column", where)
	}
	if got := args[2:]; !reflect.DeepEqual(got, []interface{}{"key-a", "key-b"}) {
		t.Fatalf("args[2:] = %#v, want the trimmed ids", got)
	}

	// The system-call entry rides along with ids as an alternative.
	where, args = buildWhereClause(LogQueryParams{
		Days:      1,
		APIKeyIDs: []string{"key-a", SystemRequestLogFilterValue},
	})
	if !strings.Contains(where, "(api_key_id IN (?) OR (trim(coalesce(api_key_name, '')) = ''") {
		t.Fatalf("where = %q, want ids OR the system-call predicate", where)
	}
	if got := args[2:]; !reflect.DeepEqual(got, []interface{}{"key-a"}) {
		t.Fatalf("args[2:] = %#v, want only the real id bound", got)
	}
}

func TestInferChannelDisplayMeta(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		label        string
		source       string
		model        string
		providerHint string
		wantProvider string
		wantAuthType string
	}{
		{
			name:         "opencode go api key",
			label:        "opencode go",
			source:       "sk-abc123xyz",
			model:        "deepseek-v4-flash",
			wantProvider: "opencode-go",
			wantAuthType: "api",
		},
		{
			name:         "oauth email codex",
			label:        "yuan364299311@gmail.com",
			source:       "yuan364299311@gmail.com",
			model:        "gpt-5.6-sol",
			wantProvider: "codex",
			wantAuthType: "oauth",
		},
		{
			name:         "historical xai by model",
			label:        "yuan364299311@gmail.com",
			source:       "yuan364299311@gmail.com",
			model:        "grok-4.5",
			wantProvider: "xai",
			wantAuthType: "oauth",
		},
		{
			name:         "live provider hint wins",
			label:        "opencode go",
			source:       "sk-abc",
			providerHint: "opencode-go",
			wantProvider: "opencode-go",
			wantAuthType: "api",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider, authType := InferChannelDisplayMeta(tc.label, tc.source, tc.model, tc.providerHint)
			if provider != tc.wantProvider || authType != tc.wantAuthType {
				t.Fatalf("got provider/authType = %q/%q, want %q/%q", provider, authType, tc.wantProvider, tc.wantAuthType)
			}
		})
	}
}
