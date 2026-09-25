package openai

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Video generation is asynchronous end to end.
//
// POST /v1/videos/generations returns the upstream request id, and the caller
// polls GET /v1/videos/{request_id} until status leaves "pending". The proxy does
// not collapse this into one blocking call: a clip takes minutes to render, far
// past the timeouts clients and intermediaries apply to a single request.
//
// Polling has to reach the same account that submitted the job — a request id is
// scoped to the credential that created it — so submissions are remembered in
// videoJobRegistry and the poll is pinned to that credential.
const (
	openAIVideoGenerationAlt = "videos/generations"
	openAIVideoStatusAlt     = "videos/status"

	defaultVideoModelID = "grok-imagine-video-1.5"
)

type OpenAIVideosAPIHandler struct {
	*handlers.BaseAPIHandler
}

func NewOpenAIVideosAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIVideosAPIHandler {
	return &OpenAIVideosAPIHandler{BaseAPIHandler: apiHandlers}
}

func (h *OpenAIVideosAPIHandler) Generations(c *gin.Context) {
	rawJSON, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		return
	}

	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if modelName == "" {
		modelName = defaultVideoModelID
		if updated, err := sjson.SetBytes(rawJSON, "model", modelName); err == nil {
			rawJSON = updated
		}
	}
	provider := openAIVideoGenerationProvider(modelName)
	if provider == "" {
		writeOpenAIImagesError(c, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("model %q is not a supported video generation model", modelName))
		return
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String()) == "" {
		writeOpenAIImagesError(c, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}

	if h.AuthManager == nil {
		writeOpenAIImagesError(c, http.StatusInternalServerError, "server_error", "authentication manager not initialized")
		return
	}

	cliCtx := ginRequestContext(c)
	meta := requestImageExecutionMetadata(c)

	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	defer stopKeepAlive()

	execMeta := cloneImageExecutionMetadata(meta)
	selected := &selectedAuthRecorder{}
	execMeta[coreexecutor.SelectedAuthCallbackMetadataKey] = selected.record
	resp, err := h.AuthManager.Execute(cliCtx, []string{provider}, coreexecutor.Request{
		Model:   modelName,
		Payload: rawJSON,
		Format:  sdktranslator.FromString("openai"),
	}, coreexecutor.Options{
		Alt:             openAIVideoGenerationAlt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata:        execMeta,
	})
	if err != nil {
		status := http.StatusBadGateway
		if statusErr, ok := err.(coreexecutor.StatusError); ok && statusErr.StatusCode() > 0 {
			status = statusErr.StatusCode()
		}
		writeOpenAIImagesError(c, status, errorTypeForStatus(status), err.Error())
		return
	}

	if requestID := strings.TrimSpace(gjson.GetBytes(resp.Payload, "request_id").String()); requestID != "" {
		route := VideoJobRoute{
			Model:    modelName,
			Provider: provider,
			AuthID:   selected.id(),
			TenantID: tenantIDFromMetadata(meta),
		}
		if errRemember := rememberVideoJob(cliCtx, requestID, route); errRemember != nil {
			// The job exists upstream either way, so the caller still gets its id;
			// polls will answer that the id is unknown rather than guess an account.
			log.WithError(errRemember).WithField("request_id", requestID).Warn("openai videos: failed to record video submission")
		}
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), resp.Headers)
	c.Data(http.StatusOK, "application/json; charset=utf-8", resp.Payload)
}

// Status serves GET /v1/videos/:request_id.
func (h *OpenAIVideosAPIHandler) Status(c *gin.Context) {
	requestID := strings.TrimSpace(c.Param("request_id"))
	if requestID == "" {
		writeOpenAIImagesError(c, http.StatusBadRequest, "invalid_request_error", "request_id is required")
		return
	}

	cliCtx := ginRequestContext(c)
	meta := requestImageExecutionMetadata(c)

	job, ok, err := lookupVideoJob(cliCtx, requestID)
	if err != nil {
		writeOpenAIImagesError(c, http.StatusServiceUnavailable, "server_error", "video request lookup is unavailable; retry shortly")
		return
	}
	// Another tenant's id is answered exactly like an unknown one, so polling
	// cannot confirm that an id exists.
	if ok && coreauth.NormalizedTenantID(job.TenantID) != coreauth.NormalizedTenantID(tenantIDFromMetadata(meta)) {
		ok = false
	}
	if !ok {
		// The mapping expires with the job, so an id from a restarted process or
		// from hours ago is genuinely unknown here. Say so rather than guessing an
		// account: polling with the wrong credential returns someone else's 404.
		writeOpenAIImagesError(c, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("video request %q is not known to this server; it may have expired", requestID))
		return
	}

	if h.AuthManager == nil {
		writeOpenAIImagesError(c, http.StatusInternalServerError, "server_error", "authentication manager not initialized")
		return
	}

	payload, err := sjson.SetBytes([]byte(`{}`), "request_id", requestID)
	if err != nil {
		writeOpenAIImagesError(c, http.StatusInternalServerError, "server_error", "encode video status request")
		return
	}

	execMeta := cloneImageExecutionMetadata(meta)
	if job.AuthID != "" {
		execMeta[coreexecutor.PinnedAuthMetadataKey] = job.AuthID
	}
	resp, err := h.AuthManager.Execute(cliCtx, []string{job.Provider}, coreexecutor.Request{
		Model:   job.Model,
		Payload: payload,
		Format:  sdktranslator.FromString("openai"),
	}, coreexecutor.Options{
		Alt:             openAIVideoStatusAlt,
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata:        execMeta,
	})
	if err != nil {
		status := http.StatusBadGateway
		if statusErr, ok := err.(coreexecutor.StatusError); ok && statusErr.StatusCode() > 0 {
			status = statusErr.StatusCode()
		}
		writeOpenAIImagesError(c, status, errorTypeForStatus(status), err.Error())
		return
	}

	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(resp.Payload, "status").String()), "done") {
		if errForget := forgetVideoJob(cliCtx, requestID); errForget != nil {
			log.WithError(errForget).WithField("request_id", requestID).Debug("openai videos: failed to drop finished video submission")
		}
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), resp.Headers)
	c.Data(http.StatusOK, "application/json; charset=utf-8", resp.Payload)
}

func tenantIDFromMetadata(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	tenant, _ := meta[coreexecutor.TenantMetadataKey].(string)
	return strings.TrimSpace(tenant)
}

// selectedAuthRecorder learns which credential the scheduler picked for a
// submission. The scheduler reports each pick through the callback; the last
// one is the credential that produced the response.
type selectedAuthRecorder struct {
	mu     sync.Mutex
	authID string
}

func (r *selectedAuthRecorder) record(authID string) {
	r.mu.Lock()
	r.authID = strings.TrimSpace(authID)
	r.mu.Unlock()
}

func (r *selectedAuthRecorder) id() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authID
}
