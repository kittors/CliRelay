package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func (e *AntigravityExecutor) newAntigravityExecutionContext(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	stream bool,
) *ExecutionContext {
	return newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:        sdktranslator.FromString("antigravity"),
		TranslateAsStream:   stream,
		PayloadConfigRoot:   "request",
		PayloadConfigFormat: "antigravity",
	})
}

func (e *AntigravityExecutor) obfuscateSensitiveWords(payload []byte) []byte {
	words := DefaultAntigravitySensitiveWords()
	if e != nil && e.cfg != nil && len(e.cfg.Antigravity.SensitiveWords) > 0 {
		words = e.cfg.Antigravity.SensitiveWords
	}
	if len(words) == 0 {
		return payload
	}
	matcher := buildSensitiveWordMatcher(words)
	return obfuscateSensitiveWordsInSystemInstruction(payload, matcher)
}

func (e *AntigravityExecutor) buildAntigravityPayload(execCtx *ExecutionContext) ([]byte, error) {
	body, originalTranslated := execCtx.TranslateRequestPair(execCtx.Request.Payload)
	body, err := thinking.ApplyThinking(body, execCtx.Request.Model, execCtx.SourceFormat.String(), execCtx.Execution.TargetFormat.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body = e.obfuscateSensitiveWords(body)
	return body, nil
}

func (e *AntigravityExecutor) buildAntigravityCountTokensPayload(execCtx *ExecutionContext) ([]byte, error) {
	body, _ := execCtx.TranslateRequestPair(execCtx.Request.Payload)
	body, err := thinking.ApplyThinking(body, execCtx.Request.Model, execCtx.SourceFormat.String(), execCtx.Execution.TargetFormat.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	body = deleteJSONField(body, "project")
	body = deleteJSONField(body, "model")
	body = deleteJSONField(body, "request.safetySettings")
	body = e.obfuscateSensitiveWords(body)
	return body, nil
}
