package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

const (
	updateHTTPTimeout     = 10 * time.Second
	updaterHealthTimeout  = 2 * time.Second
	updaterTokenEnv       = "CLIRELAY_UPDATER_TOKEN"
	githubTokenEnv        = "CLIRELAY_GITHUB_TOKEN"
	autoUpdateChannelEnv  = "CLIRELAY_UPDATE_CHANNEL"
	defaultUpdaterService = "clirelay"
)

type updateCheckResponse struct {
	Enabled           bool   `json:"enabled"`
	CurrentVersion    string `json:"current_version"`
	CurrentCommit     string `json:"current_commit"`
	CurrentUIVersion  string `json:"current_ui_version,omitempty"`
	CurrentUICommit   string `json:"current_ui_commit,omitempty"`
	BuildDate         string `json:"build_date"`
	TargetChannel     string `json:"target_channel"`
	LatestVersion     string `json:"latest_version"`
	LatestCommit      string `json:"latest_commit"`
	LatestCommitURL   string `json:"latest_commit_url,omitempty"`
	LatestUIVersion   string `json:"latest_ui_version,omitempty"`
	LatestUICommit    string `json:"latest_ui_commit,omitempty"`
	LatestUICommitURL string `json:"latest_ui_commit_url,omitempty"`
	DockerImage       string `json:"docker_image"`
	DockerTag         string `json:"docker_tag"`
	ReleaseNotes      string `json:"release_notes,omitempty"`
	ReleaseURL        string `json:"release_url,omitempty"`
	UpdateAvailable   bool   `json:"update_available"`
	UpdaterAvailable  bool   `json:"updater_available"`
	Message           string `json:"message,omitempty"`
}

type updateProgressLogEntry struct {
	Timestamp string `json:"timestamp"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

type updateProgressResponse struct {
	Status          string                   `json:"status"`
	Stage           string                   `json:"stage"`
	Message         string                   `json:"message,omitempty"`
	Service         string                   `json:"service,omitempty"`
	TargetImage     string                   `json:"target_image,omitempty"`
	TargetTag       string                   `json:"target_tag,omitempty"`
	TargetVersion   string                   `json:"target_version,omitempty"`
	TargetCommit    string                   `json:"target_commit,omitempty"`
	TargetUIVersion string                   `json:"target_ui_version,omitempty"`
	TargetUICommit  string                   `json:"target_ui_commit,omitempty"`
	TargetChannel   string                   `json:"target_channel,omitempty"`
	StartedAt       string                   `json:"started_at,omitempty"`
	UpdatedAt       string                   `json:"updated_at,omitempty"`
	FinishedAt      string                   `json:"finished_at,omitempty"`
	Logs            []updateProgressLogEntry `json:"logs,omitempty"`
}

type branchCommitInfo struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
	} `json:"commit"`
}

var (
	fetchBranchCommitForUpdateCheck      = fetchBranchCommit
	fetchLatestReleaseInfoForUpdateCheck = fetchLatestReleaseInfo
)

func (h *Handler) GetAutoUpdateEnabled(c *gin.Context) {
	enabled := true
	if h != nil && h.cfg != nil {
		enabled = h.cfg.AutoUpdate.Enabled
	}
	c.JSON(http.StatusOK, gin.H{"enabled": enabled})
}

func (h *Handler) PutAutoUpdateEnabled(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.AutoUpdate.Enabled = v })
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
	channel := normalizeAutoUpdateChannel(*body.Value)
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auto update channel"})
		return
	}
	h.cfg.AutoUpdate.Channel = channel
	h.persist(c)
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
	if !check.UpdateAvailable {
		c.JSON(http.StatusOK, gin.H{"status": "noop", "message": "already up to date"})
		return
	}

	payload := map[string]string{
		"image":      check.DockerImage,
		"tag":        check.DockerTag,
		"channel":    check.TargetChannel,
		"version":    check.LatestVersion,
		"commit":     check.LatestCommit,
		"ui_version": check.LatestUIVersion,
		"ui_commit":  check.LatestUICommit,
		"service":    updaterTargetService(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal_failed", "message": err.Error()})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, joinURLPath(resolveUpdaterURL(h.cfg), "/v1/update"), bytes.NewReader(body))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request_create_failed", "message": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token := updaterToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: updateHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "updater_unreachable", "message": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		c.JSON(http.StatusBadGateway, gin.H{"error": "updater_failed", "message": strings.TrimSpace(string(data))})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"status": "accepted", "target": check})
}

func (h *Handler) buildUpdateCheck(ctx context.Context) (*updateCheckResponse, error) {
	cfg := &config.Config{}
	if h != nil && h.cfg != nil {
		cfg = h.cfg
	}
	cfg.SanitizeAutoUpdate()

	channel := cfg.AutoUpdate.Channel
	if channel == "auto" {
		channel = inferAutoUpdateChannel(buildinfo.Version, os.Getenv(autoUpdateChannelEnv))
	}
	repo := normalizeGitHubRepository(cfg.AutoUpdate.Repository)
	frontendRepo := normalizeGitHubRepository(cfg.RemoteManagement.PanelGitHubRepository)
	client := h.githubClient()

	branch, branchErr := fetchBranchCommitForUpdateCheck(ctx, client, repo, channel)
	frontendBranch, frontendErr := fetchBranchCommitForUpdateCheck(ctx, client, frontendRepo, channel)

	release, releaseErr := fetchLatestReleaseInfoForUpdateCheck(ctx, client, repo)
	releaseNotes := strings.TrimSpace(release.Body)
	if releaseErr != nil {
		releaseNotes = ""
	}

	currentVersion := currentUpdateDisplayVersion(buildinfo.Version)
	currentCommit := strings.TrimSpace(buildinfo.Commit)
	currentUIVersion, currentUICommit := h.currentFrontendState()

	latestVersion := currentVersion
	latestCommit := currentCommit
	latestCommitURL := ""
	if branchErr == nil {
		latestVersion = latestUpdateDisplayVersion(channel, branch.SHA)
		latestCommit = strings.TrimSpace(branch.SHA)
		latestCommitURL = strings.TrimSpace(branch.HTMLURL)
	}

	latestUIVersion := currentUIVersion
	latestUICommit := currentUICommit
	latestUICommitURL := ""
	if frontendErr == nil {
		latestUIVersion = latestFrontendDisplayVersion(channel, frontendBranch.SHA)
		latestUICommit = strings.TrimSpace(frontendBranch.SHA)
		latestUICommitURL = strings.TrimSpace(frontendBranch.HTMLURL)
	}

	backendUpdateAvailable := branchErr == nil && autoUpdateAvailableFromCommit(currentCommit, branch.SHA)
	frontendUpdateAvailable := frontendErr == nil && autoUpdateAvailableFromCommit(currentUICommit, frontendBranch.SHA)
	dockerImage, dockerTag := dockerImageAndTagForConfig(cfg.AutoUpdate.DockerImage, channel, branch.SHA)

	resp := &updateCheckResponse{
		Enabled:           cfg.AutoUpdate.Enabled,
		CurrentVersion:    currentVersion,
		CurrentCommit:     currentCommit,
		CurrentUIVersion:  currentUIVersion,
		CurrentUICommit:   currentUICommit,
		BuildDate:         buildinfo.BuildDate,
		TargetChannel:     channel,
		LatestVersion:     latestVersion,
		LatestCommit:      latestCommit,
		LatestCommitURL:   latestCommitURL,
		LatestUIVersion:   latestUIVersion,
		LatestUICommit:    latestUICommit,
		LatestUICommitURL: latestUICommitURL,
		DockerImage:       dockerImage,
		DockerTag:         dockerTag,
		ReleaseNotes:      releaseNotes,
		ReleaseURL:        strings.TrimSpace(release.HTMLURL),
		UpdateAvailable:   cfg.AutoUpdate.Enabled && (backendUpdateAvailable || frontendUpdateAvailable),
		UpdaterAvailable:  checkUpdaterAvailable(ctx, cfg),
	}
	if !resp.Enabled {
		resp.Message = "auto update disabled"
	} else if branchErr != nil || frontendErr != nil {
		resp.Message = buildUpdateCheckWarning(branchErr, frontendErr)
	} else if !resp.UpdateAvailable {
		resp.Message = "already up to date"
	}
	return resp, nil
}

func (h *Handler) buildCurrentUpdateState(ctx context.Context) *updateCheckResponse {
	cfg := &config.Config{}
	if h != nil && h.cfg != nil {
		cfg = h.cfg
	}
	cfg.SanitizeAutoUpdate()

	channel := cfg.AutoUpdate.Channel
	if channel == "auto" {
		channel = inferAutoUpdateChannel(buildinfo.Version, os.Getenv(autoUpdateChannelEnv))
	}

	currentUIVersion, currentUICommit := h.currentFrontendState()
	dockerImage, dockerTag := dockerImageAndTagForConfig(cfg.AutoUpdate.DockerImage, channel, "")

	return &updateCheckResponse{
		Enabled:          cfg.AutoUpdate.Enabled,
		CurrentVersion:   currentUpdateDisplayVersion(buildinfo.Version),
		CurrentCommit:    strings.TrimSpace(buildinfo.Commit),
		CurrentUIVersion: currentUIVersion,
		CurrentUICommit:  currentUICommit,
		BuildDate:        buildinfo.BuildDate,
		TargetChannel:    channel,
		DockerImage:      dockerImage,
		DockerTag:        dockerTag,
		UpdaterAvailable: checkUpdaterAvailable(ctx, cfg),
	}
}

func (h *Handler) currentFrontendState() (string, string) {
	version := buildinfo.FrontendVersion
	ref := buildinfo.FrontendRef
	commit := strings.TrimSpace(buildinfo.FrontendCommit)

	if h != nil {
		if meta, ok := managementasset.CurrentPanelMetadata(h.configFilePath); ok {
			if meta.Version != "" {
				version = meta.Version
			}
			if meta.Ref != "" {
				ref = meta.Ref
			}
			if meta.Commit != "" {
				commit = meta.Commit
			}
		}
	}

	return currentFrontendDisplayVersion(version, ref, commit), strings.TrimSpace(commit)
}

func buildUpdateCheckWarning(branchErr error, frontendErr error) string {
	parts := make([]string, 0, 2)
	if branchErr != nil {
		parts = append(parts, "service update check degraded: "+strings.TrimSpace(branchErr.Error()))
	}
	if frontendErr != nil {
		parts = append(parts, "management UI update check degraded: "+strings.TrimSpace(frontendErr.Error()))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}

func (h *Handler) githubClient() *http.Client {
	client := &http.Client{Timeout: updateHTTPTimeout}
	if h != nil && h.cfg != nil {
		proxyURL := strings.TrimSpace(h.cfg.ProxyURL)
		if proxyURL != "" {
			util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: proxyURL}, client)
		}
	}
	return client
}

func normalizeAutoUpdateChannel(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "main", "dev", "auto":
		return strings.ToLower(strings.TrimSpace(channel))
	default:
		return ""
	}
}

func fetchBranchCommit(ctx context.Context, client *http.Client, repo string, channel string) (branchCommitInfo, error) {
	var info branchCommitInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIURL(repo, "commits/"+channel), nil)
	if err != nil {
		return info, err
	}
	applyGitHubAPIHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return info, fmt.Errorf("github commit status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return info, err
	}
	if strings.TrimSpace(info.SHA) == "" {
		return info, fmt.Errorf("github commit response missing sha")
	}
	return info, nil
}

func fetchLatestReleaseInfo(ctx context.Context, client *http.Client, repo string) (releaseInfo, error) {
	var info releaseInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIURL(repo, "releases/latest"), nil)
	if err != nil {
		return info, err
	}
	applyGitHubAPIHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return info, fmt.Errorf("github release status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

func inferAutoUpdateChannel(version string, envChannel string) string {
	env := strings.ToLower(strings.TrimSpace(envChannel))
	if env == "dev" || env == "main" {
		return env
	}
	lowered := strings.ToLower(strings.TrimSpace(version))
	if strings.HasPrefix(lowered, "dev-") || strings.Contains(lowered, "-dev") || lowered == "dev" {
		return "dev"
	}
	return "main"
}

func currentUpdateDisplayVersion(version string) string {
	trimmed := strings.TrimSpace(version)
	return trimmed
}

func latestUpdateDisplayVersion(channel string, commit string) string {
	normalized := normalizeAutoUpdateChannel(channel)
	if normalized == "dev" {
		return joinChannelCommit("dev", commit)
	}
	return joinChannelCommit("main", commit)
}

func currentFrontendDisplayVersion(version string, ref string, commit string) string {
	trimmed := strings.TrimSpace(version)
	if trimmed != "" && !strings.EqualFold(trimmed, "dev") {
		return trimmed
	}
	normalizedRef := normalizeAutoUpdateChannel(ref)
	if normalizedRef == "auto" || normalizedRef == "" {
		normalizedRef = "main"
	}
	return latestFrontendDisplayVersion(normalizedRef, commit)
}

func latestFrontendDisplayVersion(channel string, commit string) string {
	normalized := normalizeAutoUpdateChannel(channel)
	if normalized == "dev" {
		return "panel-" + joinChannelCommit("dev", commit)
	}
	return "panel-" + joinChannelCommit("main", commit)
}

func joinChannelCommit(channel string, commit string) string {
	short := shortCommit(commit)
	if short == "" {
		return channel
	}
	return channel + "-" + short
}

func shortCommit(commit string) string {
	trimmed := strings.TrimSpace(commit)
	if len(trimmed) > 7 {
		return trimmed[:7]
	}
	return trimmed
}

func autoUpdateAvailableFromCommit(currentCommit string, latestCommit string) bool {
	current := strings.TrimSpace(currentCommit)
	latest := strings.TrimSpace(latestCommit)
	if latest == "" {
		return false
	}
	if current == "" || strings.EqualFold(current, "none") {
		return true
	}
	current = strings.ToLower(current)
	latest = strings.ToLower(latest)
	return !(strings.HasPrefix(latest, current) || strings.HasPrefix(current, latest))
}

func autoUpdateAvailable(currentBackendCommit string, latestBackendCommit string, currentFrontendCommit string, latestFrontendCommit string) bool {
	return autoUpdateAvailableFromCommit(currentBackendCommit, latestBackendCommit) ||
		autoUpdateAvailableFromCommit(currentFrontendCommit, latestFrontendCommit)
}

func dockerTagForChannel(channel string, _ string) string {
	if strings.EqualFold(strings.TrimSpace(channel), "dev") {
		return "dev"
	}
	return "latest"
}

func dockerImageAndTagForConfig(image string, channel string, sha string) (string, string) {
	cleanImage := strings.TrimSpace(image)
	if cleanImage == "" {
		return cleanImage, dockerTagForChannel(channel, sha)
	}
	name, tag := splitDockerImageTag(cleanImage)
	if tag != "" {
		return name, tag
	}
	return cleanImage, dockerTagForChannel(channel, sha)
}

func splitDockerImageTag(image string) (string, string) {
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash {
		return image, ""
	}
	return image[:lastColon], image[lastColon+1:]
}

func normalizeGitHubRepository(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "kittors/CliRelay"
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Host != "" {
		trimmed = strings.Trim(parsed.Path, "/")
	}
	trimmed = strings.TrimPrefix(trimmed, "repos/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return "kittors/CliRelay"
}

func githubAPIURL(repo string, path string) string {
	return "https://api.github.com/repos/" + strings.Trim(repo, "/") + "/" + strings.TrimLeft(path, "/")
}

func applyGitHubAPIHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", latestReleaseUserAgent)
	if token := githubAPIToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func githubAPIToken() string {
	if token := strings.TrimSpace(os.Getenv(githubTokenEnv)); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

func resolveUpdaterURL(cfg *config.Config) string {
	if fromEnv := strings.TrimSpace(os.Getenv("CLIRELAY_UPDATER_URL")); fromEnv != "" {
		return fromEnv
	}
	if cfg != nil && cfg.AutoUpdate.UpdaterURL != "" {
		return cfg.AutoUpdate.UpdaterURL
	}
	return config.DefaultAutoUpdateUpdaterURL
}

func updaterToken() string {
	return strings.TrimSpace(os.Getenv(updaterTokenEnv))
}

func updaterTargetService() string {
	if service := strings.TrimSpace(os.Getenv("CLIRELAY_TARGET_SERVICE")); service != "" {
		return service
	}
	return defaultUpdaterService
}

func checkUpdaterAvailable(ctx context.Context, cfg *config.Config) bool {
	ctx, cancel := context.WithTimeout(ctx, updaterHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURLPath(resolveUpdaterURL(cfg), "/v1/health"), nil)
	if err != nil {
		return false
	}
	if token := updaterToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (h *Handler) fetchUpdateProgress(ctx context.Context) (*updateProgressResponse, error) {
	var cfg *config.Config
	if h != nil {
		cfg = h.cfg
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURLPath(resolveUpdaterURL(cfg), "/v1/status"), nil)
	if err != nil {
		return nil, err
	}
	if token := updaterToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: updateHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("updater status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var payload updateProgressResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return &payload, nil
}

func joinURLPath(base string, path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if trimmed == "" {
		trimmed = config.DefaultAutoUpdateUpdaterURL
	}
	return trimmed + "/" + strings.TrimLeft(path, "/")
}
