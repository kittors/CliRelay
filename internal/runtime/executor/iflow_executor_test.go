package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func TestIFlowExecutorParseSuffix(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantBase  string
		wantLevel string
	}{
		{"no suffix", "glm-4", "glm-4", ""},
		{"glm with suffix", "glm-4.1-flash(high)", "glm-4.1-flash", "high"},
		{"minimax no suffix", "minimax-m2", "minimax-m2", ""},
		{"minimax with suffix", "minimax-m2.1(medium)", "minimax-m2.1", "medium"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := thinking.ParseSuffix(tt.model)
			if result.ModelName != tt.wantBase {
				t.Errorf("ParseSuffix(%q).ModelName = %q, want %q", tt.model, result.ModelName, tt.wantBase)
			}
		})
	}
}

func TestPreserveReasoningContentInMessages(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  []byte // nil means output should equal input
	}{
		{
			"non-glm model passthrough",
			[]byte(`{"model":"gpt-4","messages":[]}`),
			nil,
		},
		{
			"glm model with empty messages",
			[]byte(`{"model":"glm-4","messages":[]}`),
			nil,
		},
		{
			"glm model preserves existing reasoning_content",
			[]byte(`{"model":"glm-4","messages":[{"role":"assistant","content":"hi","reasoning_content":"thinking..."}]}`),
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preserveReasoningContentInMessages(tt.input)
			want := tt.want
			if want == nil {
				want = tt.input
			}
			if string(got) != string(want) {
				t.Errorf("preserveReasoningContentInMessages() = %s, want %s", got, want)
			}
		})
	}
}

func TestIFlowCookieRefreshLogsMaskedEmail(t *testing.T) {
	var buf bytes.Buffer
	previousOutput := log.StandardLogger().Out
	previousLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetLevel(previousLevel)
	})

	email := "user@example.com"
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"expired": time.Now().Add(72 * time.Hour).Format("2006-01-02 15:04"),
	}}

	gotAuth, err := (&IFlowExecutor{}).refreshCookieBased(context.Background(), auth, "BXAuth=secret;", email)
	if err != nil {
		t.Fatalf("refreshCookieBased returned error: %v", err)
	}
	if gotAuth != auth {
		t.Fatalf("refreshCookieBased returned different auth when refresh was not needed")
	}

	got := buf.String()
	if strings.Contains(got, email) {
		t.Fatalf("refreshCookieBased log leaked full email: %s", got)
	}
	if masked := util.HideAPIKey(email); !strings.Contains(got, masked) {
		t.Fatalf("refreshCookieBased log = %q, want masked email %q", got, masked)
	}
}
