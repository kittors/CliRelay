package usage

import (
	"reflect"
	"testing"
)

func TestCcSwitchClientTypesKeepDisplayOrderAndAreACopy(t *testing.T) {
	want := []string{"claude", "codex", "gemini", "opencode", "openclaw", "hermes"}
	got := CcSwitchClientTypes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CcSwitchClientTypes() = %#v, want %#v", got, want)
	}
	got[0] = "mutated"
	if CcSwitchClientTypes()[0] != "claude" {
		t.Fatal("callers can mutate the shared client-type list")
	}
}

func TestIsCcSwitchClientType(t *testing.T) {
	for _, value := range []string{"claude", "codex", "gemini", "opencode", "openclaw", "hermes", " Hermes ", "OPENCODE"} {
		if !IsCcSwitchClientType(value) {
			t.Errorf("IsCcSwitchClientType(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "pi", "grokbuild", "claude-desktop", "open code"} {
		if IsCcSwitchClientType(value) {
			t.Errorf("IsCcSwitchClientType(%q) = true, want false", value)
		}
	}
}

func TestCcSwitchClientUsesOpenAIProtocol(t *testing.T) {
	cases := map[string]bool{
		"codex":    true,
		"opencode": true,
		"openclaw": true,
		" Hermes ": true,
		"claude":   false,
		"gemini":   false,
		"":         false,
	}
	for clientType, want := range cases {
		if got := CcSwitchClientUsesOpenAIProtocol(clientType); got != want {
			t.Errorf("CcSwitchClientUsesOpenAIProtocol(%q) = %v, want %v", clientType, got, want)
		}
	}
}
