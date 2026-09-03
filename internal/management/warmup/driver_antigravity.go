package warmup

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// AntigravityDriver handles quota warmup for Antigravity accounts.
// Antigravity has two primary sliding window pools:
// 1. Gemini Models ("antigravity:gemini"): triggered via minimal flash request.
// 2. 3P / Claude Models ("antigravity:3p"): triggered via minimal claude request.
type AntigravityDriver struct {
	cfg      *config.Config
	executor *executor.AntigravityExecutor
}

func NewAntigravityDriver(cfg *config.Config) *AntigravityDriver {
	return &AntigravityDriver{
		cfg:      cfg,
		executor: executor.NewAntigravityExecutor(cfg),
	}
}

func (d *AntigravityDriver) Provider() string {
	return "antigravity"
}

func (d *AntigravityDriver) GetTargets(auth *coreauth.Auth) []Target {
	return []Target{
		{
			PoolID:      "antigravity:gemini",
			PoolLabel:   "Gemini Models",
			TargetModel: "gemini-2.5-flash",
			Window:      5 * time.Hour,
		},
		{
			PoolID:      "antigravity:3p",
			PoolLabel:   "Claude & GPT Models",
			TargetModel: "claude-sonnet-4-6",
			Window:      5 * time.Hour,
		},
	}
}

func (d *AntigravityDriver) ExecuteWarmup(ctx context.Context, auth *coreauth.Auth, target Target) (*Result, error) {
	start := time.Now()
	res := &Result{
		AuthID:      auth.ID,
		PoolID:      target.PoolID,
		TargetModel: target.TargetModel,
		TriggeredAt: start,
	}

	// Minimal 1-token prompt payload
	minimalPayload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"1"}]}],"generationConfig":{"maxOutputTokens":1}}}`)

	execReq := cliproxyexecutor.Request{
		Model:   target.TargetModel,
		Payload: minimalPayload,
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("antigravity"),
	}

	execResp, err := d.executor.Execute(ctx, auth, execReq, opts)
	res.Latency = time.Since(start)

	if err != nil {
		res.Success = false
		res.ErrorMessage = err.Error()
		if strings.Contains(err.Error(), "429") {
			res.StatusCode = 429
		} else if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
			res.StatusCode = 401
		} else {
			res.StatusCode = 500
		}
		return res, nil
	}

	res.Success = true
	res.StatusCode = 200
	_ = execResp
	return res, nil
}
