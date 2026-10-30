package middleware

import "testing"

func TestShouldLogRequestSkipsSecretsAndScrapes(t *testing.T) {
	cases := map[string]bool{
		"/v1/chat/completions":       true,
		"/v0/management/config.yaml": false,
		"/management.html":           false,
		"/api/provider/openai/v1/x":  true,
		"/api/other":                 false,
		// A scrape carries a bearer token and repeats every few seconds.
		"/metrics":                     false,
		"/metrics-dashboard-lookalike": true,
	}
	for path, want := range cases {
		if got := shouldLogRequest(path); got != want {
			t.Errorf("shouldLogRequest(%q) = %v, want %v", path, got, want)
		}
	}
}
