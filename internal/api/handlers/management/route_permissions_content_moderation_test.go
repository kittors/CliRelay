package management

import (
	"net/http"
	"testing"
)

func TestContentModerationTestRoutesAreMatchedWhole(t *testing.T) {
	tests := []struct {
		method, relative string
		want             bool
	}{
		{http.MethodPost, "/content-moderation/word-policies/test", true},
		{http.MethodPost, "/content-moderation/word-policies/test/", true},
		{http.MethodPost, "/content-moderation/profiles/p-1/test", true},
		// The same paths with another method are not a dry run.
		{http.MethodGet, "/content-moderation/word-policies/test", false},
		{http.MethodPut, "/content-moderation/word-policies/test", false},
		{http.MethodDelete, "/content-moderation/profiles/p-1/test", false},
		// A route that only ends in "test" is not one of them.
		{http.MethodPut, "/content-moderation/word-policies/keys/test", false},
		{http.MethodDelete, "/content-moderation/word-policies/keys/test", false},
		{http.MethodGet, "/content-moderation/word-policies/keys/test", false},
		{http.MethodPost, "/content-moderation/word-policies/keys/test", false},
		{http.MethodPost, "/content-moderation/word-policies/tenant/test", false},
		{http.MethodPost, "/content-moderation/profiles/test", false},
		{http.MethodPost, "/content-moderation/profiles//test", false},
		{http.MethodPost, "/content-moderation/profiles/a/b/test", false},
		{http.MethodPost, "/content-moderation/metrics/test", false},
	}
	for _, tt := range tests {
		got := tt.method == http.MethodPost && isContentModerationTestRoute(tt.relative)
		if got != tt.want {
			t.Errorf("%s %s: test route = %v, want %v", tt.method, tt.relative, got, tt.want)
		}
		permission := permissionForManagementRequest(tt.method, "/v0/management"+tt.relative)
		if (permission == "content_moderation.test") != tt.want {
			t.Errorf("%s %s: permission = %q, want the test permission = %v", tt.method, tt.relative, permission, tt.want)
		}
	}
}
