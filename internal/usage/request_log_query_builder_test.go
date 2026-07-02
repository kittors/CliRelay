package usage

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestBuildWhereClauseUsesParameterizedFilters(t *testing.T) {
	where, args := buildWhereClause(LogQueryParams{
		Days:                  3,
		APIKeys:               []string{" sk-a ", "sk-b", "SK-B"},
		Models:                []string{" gpt-5 ", "gpt-4"},
		Statuses:              []string{"failed"},
		AuthIndexes:           []string{" auth-1 ", ""},
		AuthIndexChannelNames: map[string][]string{" auth-2 ": {" Legacy ", ""}},
		ChannelNames:          []string{" Codex ", ""},
	})

	wantWhere := " WHERE timestamp >= ? AND (api_key = ? OR api_key = ?) AND model IN (?,?) AND failed = 1 AND ((auth_index IN (?) AND trim(coalesce(channel_name, '')) = '') OR (auth_index = ? AND lower(trim(channel_name)) IN (?)) OR lower(trim(channel_name)) IN (?))"
	if where != wantWhere {
		t.Fatalf("where = %q, want %q", where, wantWhere)
	}
	if len(args) == 0 {
		t.Fatal("expected cutoff argument")
	}
	cutoff, ok := args[0].(string)
	if !ok {
		t.Fatalf("cutoff arg type = %T, want string", args[0])
	}
	if _, err := time.Parse(time.RFC3339, cutoff); err != nil {
		t.Fatalf("cutoff arg %q is not RFC3339: %v", cutoff, err)
	}

	wantArgs := []interface{}{"sk-a", "sk-b", "gpt-5", "gpt-4", "auth-1", "auth-2", "legacy", "codex"}
	if !reflect.DeepEqual(args[1:], wantArgs) {
		t.Fatalf("args[1:] = %#v, want %#v", args[1:], wantArgs)
	}
}

func TestBuildWhereClauseSupportsSystemRequestFilter(t *testing.T) {
	where, args := buildWhereClause(LogQueryParams{
		Days:    1,
		APIKeys: []string{systemRequestLogFilterValue},
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
	if len(args) != 1 {
		t.Fatalf("system filter args = %#v, want only cutoff", args)
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
	wantClause := " WHERE (api_key_id = ? OR (trim(coalesce(api_key_id, '')) = '' AND api_key = ?))"
	if clause != wantClause {
		t.Fatalf("clause = %q, want %q", clause, wantClause)
	}
	wantArgs := []interface{}{"stable-key-1", "sk-live"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}

	clause, args = buildSingleAPIKeySelectorClause(" missing-key ")
	if clause != " WHERE api_key = ?" {
		t.Fatalf("missing key clause = %q, want raw api_key selector", clause)
	}
	wantArgs = []interface{}{"missing-key"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("missing key args = %#v, want %#v", args, wantArgs)
	}
}
