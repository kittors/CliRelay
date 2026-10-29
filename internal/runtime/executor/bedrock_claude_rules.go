package executor

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

// bedrockCatalogModel returns the Claude catalog id behind the first of ids
// that names one, or "". Bedrock requests name models by catalog id
// ("claude-opus-4-7") or by Bedrock id ("us.anthropic.claude-opus-4-7-v1"), and
// what the catalog says about the model decides how the request has to look:
// without this a Bedrock id resolves to nothing and a budget reaches a model
// that only takes adaptive thinking.
func bedrockCatalogModel(ids ...string) string {
	for _, id := range ids {
		if info := registry.LookupClaudeModelInfo(id); info != nil {
			return info.ID
		}
		for _, candidate := range sdkmodelcatalog.ClaudeModelCandidatesForBedrockID(id) {
			if info := registry.LookupClaudeModelInfo(candidate); info != nil {
				return info.ID
			}
		}
	}
	return ""
}

// bedrockThinkingModel is the model name ApplyThinking looks up: the catalog id
// when the requested name is a Bedrock id, with the requested thinking suffix
// kept.
func bedrockThinkingModel(requested, catalogModel string) string {
	parsed := thinking.ParseSuffix(requested)
	if catalogModel == "" || catalogModel == parsed.ModelName {
		return requested
	}
	if parsed.HasSuffix {
		return catalogModel + "(" + parsed.RawSuffix + ")"
	}
	return catalogModel
}

// bedrockInvokeResult is the outcome of invokeClaude.
type bedrockInvokeResult struct {
	// resp is the response of the last attempt. On a non-2xx status its body
	// has already been read into errBody and closed.
	resp    *http.Response
	errBody []byte
	// claudeBody is the Claude-shaped request of the last attempt, before
	// the Bedrock cleanup, for translating the response.
	claudeBody []byte
}

func (r bedrockInvokeResult) failed() bool {
	return r.resp.StatusCode < http.StatusOK || r.resp.StatusCode >= http.StatusMultipleChoices
}

// invokeClaude sends body, a Claude Messages request, to modelID on Bedrock,
// shaped for every rule plan knows for the model, and handles a 400 that names
// another rule the request broke the way sendClaudeMessages does. Bedrock passes
// Anthropic's validation messages through, so the same rules apply.
func (e *BedrockExecutor) invokeClaude(execCtx *ExecutionContext, modelID string, stream bool, body []byte, betaTokens []string, plan *claudeRequestPlan) (bedrockInvokeResult, error) {
	body = plan.begin(execCtx, body)
	for {
		// A model known to take adaptive thinking only, from the catalog
		// or from its own 400, keeps output_config.effort.
		prepared, err := prepareBedrockRequestBody(body, modelID, betaTokens, plan.knownAdaptive())
		if err != nil {
			return bedrockInvokeResult{claudeBody: body}, err
		}
		httpResp, err := e.doBedrockRequest(execCtx, modelID, stream, prepared)
		if err != nil {
			return bedrockInvokeResult{claudeBody: body}, err
		}
		recorder := execCtx.Recorder()
		recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
		result := bedrockInvokeResult{resp: httpResp, claudeBody: body}
		if !result.failed() {
			return result, nil
		}
		result.errBody = readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(result.errBody)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("bedrock executor: close response body error: %v", errClose)
		}
		retryBody, ok := plan.retry(execCtx, httpResp.StatusCode, result.errBody)
		if !ok {
			return result, nil
		}
		body = retryBody
	}
}
