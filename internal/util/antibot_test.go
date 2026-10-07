package util

import (
	"net/http"
	"testing"
)

func TestIsAntiBotChallenge(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		body   string
		want   bool
	}{
		{"cloudflare interstitial", nil, `<!DOCTYPE html><html><head><title>Just a moment...</title>`, true},
		{"challenge script", nil, `<html><script src="/cdn-cgi/challenge-platform/h/b/orchestrate"></script>`, true},
		{"mitigation header", http.Header{"Cf-Mitigated": {"challenge"}}, ``, true},
		{"json error", nil, `{"type":"error","error":{"type":"permission_error"}}`, false},
		{"plain html error", nil, `<html><body>Internal error</body></html>`, false},
		{"empty", nil, ``, false},
	}
	for _, tc := range cases {
		if got := IsAntiBotChallenge(tc.header, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: IsAntiBotChallenge = %v, want %v", tc.name, got, tc.want)
		}
	}
}
