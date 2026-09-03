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

// CodexDriver handles warmup for Codex (OpenAI) accounts.
type CodexDriver struct {
	cfg      *config.Config
	executor *executor.CodexExecutor
}

func NewCodexDriver(cfg *config.Config) *CodexDriver {
	return &CodexDriver{
		cfg:      cfg,
		executor: executor.NewCodexExecutor(cfg),
	}
}

func (d *CodexDriver) Provider() string {
	return "codex"
}

func (d *CodexDriver) GetTargets(auth *coreauth.Auth) []Target {
	return []Target{
		{
			PoolID:      "codex:5h",
			PoolLabel:   "Codex (5h Pool)",
			TargetModel: "gpt-5.3-codex-spark",
			Window:      5 * time.Hour,
		},
	}
}

func (d *CodexDriver) ExecuteWarmup(ctx context.Context, auth *coreauth.Auth, target Target) (*Result, error) {
	start := time.Now()
	res := &Result{
		AuthID:      auth.ID,
		PoolID:      target.PoolID,
		TargetModel: target.TargetModel,
		TriggeredAt: start,
	}

	minimalPayload := []byte(`{"model":"` + target.TargetModel + `","messages":[{"role":"user","content":"1"}],"max_tokens":1}`)
	execReq := cliproxyexecutor.Request{
		Model:   target.TargetModel,
		Payload: minimalPayload,
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
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
