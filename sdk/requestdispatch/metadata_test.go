package requestdispatch

import (
	"context"
	"reflect"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestBuildExecutionMetadataForwardsStrictAuthAllowlist(t *testing.T) {
	t.Parallel()

	if _, ok := BuildExecutionMetadata(context.Background(), MetadataAccessors{})[coreexecutor.AllowedAuthIDsMetadataKey]; ok {
		t.Fatal("allowlist metadata set without an accessor")
	}
	unset := MetadataAccessors{AllowedAuthIDs: func(context.Context) ([]string, bool) { return nil, false }}
	if _, ok := BuildExecutionMetadata(context.Background(), unset)[coreexecutor.AllowedAuthIDsMetadataKey]; ok {
		t.Fatal("allowlist metadata set when the accessor reports none")
	}

	source := []string{"account-a"}
	set := MetadataAccessors{AllowedAuthIDs: func(context.Context) ([]string, bool) { return source, true }}
	got, ok := BuildExecutionMetadata(context.Background(), set)[coreexecutor.AllowedAuthIDsMetadataKey].([]string)
	if !ok || !reflect.DeepEqual(got, []string{"account-a"}) {
		t.Fatalf("allowlist metadata = %#v", got)
	}
	got[0] = "mutated"
	if source[0] != "account-a" {
		t.Fatal("metadata shares backing storage with the accessor result")
	}

	empty := MetadataAccessors{AllowedAuthIDs: func(context.Context) ([]string, bool) { return nil, true }}
	got, ok = BuildExecutionMetadata(context.Background(), empty)[coreexecutor.AllowedAuthIDsMetadataKey].([]string)
	if !ok || got == nil || len(got) != 0 {
		t.Fatalf("empty allowlist metadata = %#v ok=%v, want present and empty", got, ok)
	}
}
