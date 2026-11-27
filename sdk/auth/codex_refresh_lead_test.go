package auth

import (
	"testing"
	"time"
)

func TestCodexAuthenticatorRefreshLead(t *testing.T) {
	lead := NewCodexAuthenticator().RefreshLead()
	if lead == nil {
		t.Fatal("CodexAuthenticator.RefreshLead() returned nil")
	}
	if *lead != 24*time.Hour {
		t.Fatalf("CodexAuthenticator.RefreshLead() = %v, want 24h", *lead)
	}
}
