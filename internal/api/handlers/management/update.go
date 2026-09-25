package management

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementupdate "github.com/router-for-me/CLIProxyAPI/v6/internal/management/updateflow"
)

func (h *Handler) GetAutoUpdateEnabled(c *gin.Context) {
	enabled := true
	if h != nil && h.cfg != nil {
		enabled = h.cfg.AutoUpdate.Enabled
	}
	c.JSON(http.StatusOK, gin.H{"enabled": enabled})
}

// PutAutoUpdateEnabled changes auto-update in this node's config.yaml only:
// auto-update drives the updater sidecar of the node itself, so it is a
// per-node setting even in a cluster.
func (h *Handler) PutAutoUpdateEnabled(c *gin.Context) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	h.cfg.AutoUpdate.Enabled = *body.Value
	h.mu.Unlock()
	h.persistNodeLocal(c)
}

func (h *Handler) GetAutoUpdateChannel(c *gin.Context) {
	channel := config.DefaultAutoUpdateChannel
	if h != nil && h.cfg != nil {
		h.cfg.SanitizeAutoUpdate()
		channel = h.cfg.AutoUpdate.Channel
	}
	c.JSON(http.StatusOK, gin.H{"channel": channel})
}

func (h *Handler) PutAutoUpdateChannel(c *gin.Context) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	channel := managementupdate.NormalizeAutoUpdateChannel(*body.Value)
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auto update channel"})
		return
	}
	h.mu.Lock()
	h.cfg.AutoUpdate.Channel = channel
	h.mu.Unlock()
	h.persistNodeLocal(c)
}

func (h *Handler) CheckUpdate(c *gin.Context) {
	resp, err := h.buildUpdateCheck(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_check_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) GetCurrentUpdateState(c *gin.Context) {
	c.JSON(http.StatusOK, h.buildCurrentUpdateState(c.Request.Context()))
}

func (h *Handler) GetUpdateProgress(c *gin.Context) {
	progress, err := h.fetchUpdateProgress(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_progress_failed", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, progress)
}

func (h *Handler) StreamUpdateProgress(c *gin.Context) {
	// Accept the resume point from either the SSE header or a query parameter. The
	// panel reconnects with fetch rather than EventSource and finds the query
	// parameter easier to attach, but real EventSource clients only send the header.
	lastEventID := c.GetHeader("Last-Event-ID")
	if strings.TrimSpace(lastEventID) == "" {
		lastEventID = c.Query("last_event_id")
	}
	upstream, err := h.updateService().OpenProgressStream(c.Request.Context(), lastEventID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_events_failed", "message": err.Error()})
		return
	}
	defer upstream.Body.Close()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	buffer := make([]byte, 4096)
	for {
		n, readErr := upstream.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				return
			}
			c.Writer.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF && c.Request.Context().Err() == nil {
				_ = c.Error(readErr)
			}
			return
		}
	}
}

func (h *Handler) ApplyUpdate(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config_unavailable"})
		return
	}
	if !h.cfg.AutoUpdate.Enabled {
		c.JSON(http.StatusForbidden, gin.H{"error": "auto_update_disabled"})
		return
	}

	check, err := h.buildUpdateCheck(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_check_failed", "message": err.Error()})
		return
	}
	if check.UpdateAvailable && !check.UpdaterAvailable {
		message := strings.TrimSpace(check.UpdaterHealthMessage)
		if message == "" {
			message = "updater is unavailable"
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "updater_unavailable", "message": message})
		return
	}
	if !check.UpdateAvailable {
		message := strings.TrimSpace(check.Message)
		if message == "" {
			message = "already up to date"
		}
		c.JSON(http.StatusOK, gin.H{"status": "noop", "message": message})
		return
	}

	trigger, err := h.updateService().TriggerUpdate(c.Request.Context(), check)
	if err != nil {
		msg := strings.TrimSpace(err.Error())
		switch {
		case strings.HasPrefix(msg, "marshal_failed:"):
			c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal_failed", "message": strings.TrimSpace(strings.TrimPrefix(msg, "marshal_failed:"))})
		case strings.HasPrefix(msg, "request_create_failed:"):
			c.JSON(http.StatusInternalServerError, gin.H{"error": "request_create_failed", "message": strings.TrimSpace(strings.TrimPrefix(msg, "request_create_failed:"))})
		case strings.HasPrefix(msg, "updater_unreachable:"):
			c.JSON(http.StatusBadGateway, gin.H{"error": "updater_unreachable", "message": strings.TrimSpace(strings.TrimPrefix(msg, "updater_unreachable:"))})
		case strings.HasPrefix(msg, "updater_failed:"):
			c.JSON(http.StatusBadGateway, gin.H{"error": "updater_failed", "message": strings.TrimSpace(strings.TrimPrefix(msg, "updater_failed:"))})
		case strings.HasPrefix(msg, "updater_response_invalid:"):
			c.JSON(http.StatusBadGateway, gin.H{"error": "updater_response_invalid", "message": strings.TrimSpace(strings.TrimPrefix(msg, "updater_response_invalid:"))})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update_apply_failed", "message": msg})
		}
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"status": "accepted", "run_id": trigger.RunID, "target": check})
}

func (h *Handler) buildUpdateCheck(ctx context.Context) (*updateCheckResponse, error) {
	return h.updateService().BuildUpdateCheck(ctx)
}

func (h *Handler) buildCurrentUpdateState(ctx context.Context) *updateCheckResponse {
	return h.updateService().BuildCurrentUpdateState(ctx)
}

func (h *Handler) currentFrontendState() (string, string) {
	return h.updateService().CurrentFrontendState()
}

func (h *Handler) fetchUpdateProgress(ctx context.Context) (*updateProgressResponse, error) {
	return h.updateService().FetchProgress(ctx)
}

func (h *Handler) updateService() *managementupdate.Service {
	var (
		cfg            *config.Config
		configFilePath string
	)
	if h != nil {
		cfg = h.cfg
		configFilePath = h.configFilePath
	}
	return managementupdate.New(cfg, managementupdate.Dependencies{
		CurrentVersion:         buildinfo.Version,
		CurrentCommit:          buildinfo.Commit,
		BuildDate:              buildinfo.BuildDate,
		CurrentFrontendVersion: buildinfo.FrontendVersion,
		CurrentFrontendCommit:  buildinfo.FrontendCommit,
		CurrentFrontendRef:     buildinfo.FrontendRef,
		ConfigFilePath:         configFilePath,
		FetchBranchCommit:      fetchBranchCommitForUpdateCheck,
		FetchLatestReleaseInfo: func(ctx context.Context, client *http.Client, repo string) (managementupdate.ReleaseInfo, error) {
			info, err := fetchLatestReleaseInfoForUpdateCheck(ctx, client, repo)
			if err != nil {
				return managementupdate.ReleaseInfo{}, err
			}
			return managementupdate.ReleaseInfo{
				TagName:     info.TagName,
				Name:        info.Name,
				Body:        info.Body,
				HTMLURL:     info.HTMLURL,
				PublishedAt: info.PublishedAt,
			}, nil
		},
		FetchSuccessfulWorkflowRun: fetchLatestSuccessfulWorkflowRunForUpdateCheck,
	})
}
