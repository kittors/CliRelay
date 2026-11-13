package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestProviderAccessFrozenReachesClientsWithItsCodeAndNotice(t *testing.T) {
	notice := "Access to Codex is frozen for this account since 2026-11-13 18:17 +08:00. Contact your administrator to restore it."
	err := fmt.Errorf("pick: %w", &coreauth.Error{Code: coreauth.ProviderAccessFrozenCode, Message: notice, HTTPStatus: http.StatusForbidden})

	for name, msg := range map[string]*StreamErrorMessage{
		"execution": executionError(err),
		"stream":    streamErrorMessageFromErr(err),
	} {
		if msg == nil || msg.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: message = %#v, want 403", name, msg)
		}
		status, body := FormatErrorBody(ErrorFormatOpenAI, msg.StatusCode, msg.Error.Error())
		if status != http.StatusForbidden {
			t.Fatalf("%s: OpenAI status = %d", name, status)
		}
		// Not insufficient_quota: Codex would show its own quota message instead.
		if got := gjson.GetBytes(body, "error.code").String(); got != coreauth.ProviderAccessFrozenCode {
			t.Fatalf("%s: OpenAI code = %q in %s", name, got, body)
		}
		if got := gjson.GetBytes(body, "error.message").String(); got != notice {
			t.Fatalf("%s: OpenAI message = %q", name, got)
		}
		_, claude := FormatErrorBody(ErrorFormatClaude, msg.StatusCode, msg.Error.Error())
		if got := gjson.GetBytes(claude, "error.message").String(); !strings.HasPrefix(got, notice) || !strings.Contains(got, coreauth.ProviderAccessFrozenCode) {
			t.Fatalf("%s: Claude message = %q", name, got)
		}
	}

	if providerAccessFrozenMessage(errors.New("other")) != nil {
		t.Fatal("an ordinary error was taken for a freeze")
	}
	if providerAccessFrozenMessage(&coreauth.Error{Code: "auth_not_found", Message: "no auth available"}) != nil {
		t.Fatal("another selection error was taken for a freeze")
	}
}
