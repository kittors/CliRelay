package warmup

import (
	"context"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Target describes a single quota pool target within an account.
type Target struct {
	PoolID       string        `json:"pool_id"`       // e.g. "antigravity:gemini", "antigravity:3p", "codex:default"
	PoolLabel    string        `json:"pool_label"`    // e.g. "Gemini Models", "Claude & GPT", "Codex (Spark)"
	TargetModel  string        `json:"target_model"`  // Minimal probe model (e.g. "gemini-2.5-flash", "claude-sonnet-4-6")
	Window       time.Duration `json:"window"`        // Window duration (e.g. 5 hours)
}

// Result describes the outcome of a warmup execution.
type Result struct {
	AuthID        string        `json:"auth_id"`
	PoolID        string        `json:"pool_id"`
	TargetModel   string        `json:"target_model"`
	Success       bool          `json:"success"`
	StatusCode    int           `json:"status_code"`
	Latency       time.Duration `json:"latency"`
	ErrorMessage  string        `json:"error_message,omitempty"`
	TriggeredAt   time.Time     `json:"triggered_at"`
}

// Driver defines how a specific AI provider performs minimal 1-token warmup calls.
type Driver interface {
	Provider() string
	GetTargets(auth *coreauth.Auth) []Target
	ExecuteWarmup(ctx context.Context, auth *coreauth.Auth, target Target) (*Result, error)
}
