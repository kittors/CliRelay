package updateflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type Dependencies struct {
	CurrentVersion             string
	CurrentCommit              string
	BuildDate                  string
	CurrentFrontendVersion     string
	CurrentFrontendCommit      string
	CurrentFrontendRef         string
	ConfigFilePath             string
	FetchBranchCommit          func(context.Context, *http.Client, string, string) (BranchCommitInfo, error)
	FetchLatestReleaseInfo     func(context.Context, *http.Client, string) (ReleaseInfo, error)
	FetchSuccessfulWorkflowRun func(context.Context, *http.Client, string, string, string) (WorkflowRunInfo, error)
}

type Service struct {
	cfg  *config.Config
	deps Dependencies
}

func New(cfg *config.Config, deps Dependencies) *Service {
	return &Service{cfg: cfg, deps: deps}
}

func (s *Service) BuildUpdateCheck(ctx context.Context) (*CheckResponse, error) {
	cfg := s.config()
	cfg.SanitizeAutoUpdate()

	channel := cfg.AutoUpdate.Channel
	if channel == "auto" {
		channel = InferAutoUpdateChannel(s.deps.CurrentVersion, os.Getenv(AutoUpdateChannelEnv))
	}
	repo := NormalizeGitHubRepository(cfg.AutoUpdate.Repository)
	frontendRepo := NormalizeGitHubRepository(cfg.RemoteManagement.PanelGitHubRepository)
	client := s.githubClient()

	branch, branchErr := s.fetchBranchCommit(ctx, client, repo, channel)
	frontendBranch, frontendErr := s.fetchBranchCommit(ctx, client, frontendRepo, channel)

	release, releaseErr := s.fetchLatestReleaseInfo(ctx, client, repo)
	releaseNotes := strings.TrimSpace(release.Body)
	if releaseErr != nil {
		releaseNotes = ""
	}

	currentVersion := CurrentUpdateDisplayVersion(s.deps.CurrentVersion)
	currentCommit := strings.TrimSpace(s.deps.CurrentCommit)
	currentUIVersion, currentUICommit := s.CurrentFrontendState()

	latestVersion := currentVersion
	latestCommit := currentCommit
	latestCommitURL := ""
	if branchErr == nil {
		latestVersion = LatestUpdateDisplayVersion(channel, branch.SHA)
		latestCommit = strings.TrimSpace(branch.SHA)
		latestCommitURL = strings.TrimSpace(branch.HTMLURL)
	}

	latestUIVersion := currentUIVersion
	latestUICommit := currentUICommit
	latestUICommitURL := ""
	if frontendErr == nil {
		latestUIVersion = LatestFrontendDisplayVersion(channel, frontendBranch.SHA)
		latestUICommit = strings.TrimSpace(frontendBranch.SHA)
		latestUICommitURL = strings.TrimSpace(frontendBranch.HTMLURL)
	}

	backendUpdateAvailable := branchErr == nil && AutoUpdateAvailableFromCommit(currentCommit, branch.SHA)
	frontendUpdateAvailable := frontendErr == nil && AutoUpdateAvailableFromCommit(currentUICommit, frontendBranch.SHA)
	rawUpdateAvailable := backendUpdateAvailable || frontendUpdateAvailable

	dockerPublishReady := true
	dockerPublishMessage := ""
	if cfg.AutoUpdate.Enabled && rawUpdateAvailable && branchErr == nil && frontendErr == nil && ShouldVerifyDockerPublish(cfg, repo) {
		if err := s.verifyDockerPublishReady(ctx, client, repo, channel, branch, frontendBranch); err != nil {
			dockerPublishReady = false
			dockerPublishMessage = err.Error()
		}
	}
	updaterHealth := CheckUpdaterHealth(ctx, cfg)

	resp := &CheckResponse{
		Enabled:              cfg.AutoUpdate.Enabled,
		CurrentVersion:       currentVersion,
		CurrentCommit:        currentCommit,
		CurrentUIVersion:     currentUIVersion,
		CurrentUICommit:      currentUICommit,
		BuildDate:            s.depsCurrentBuildDate(),
		TargetChannel:        channel,
		LatestVersion:        latestVersion,
		LatestCommit:         latestCommit,
		LatestCommitURL:      latestCommitURL,
		LatestUIVersion:      latestUIVersion,
		LatestUICommit:       latestUICommit,
		LatestUICommitURL:    latestUICommitURL,
		DockerImage:          cfg.AutoUpdate.DockerImage,
		DockerTag:            DockerTagForChannel(channel, branch.SHA),
		ReleaseName:          strings.TrimSpace(release.Name),
		ReleaseTag:           strings.TrimSpace(release.TagName),
		ReleaseNotes:         releaseNotes,
		ReleaseURL:           strings.TrimSpace(release.HTMLURL),
		ReleasePublishedAt:   formatReleaseTime(release.PublishedAt),
		UpdateAvailable:      cfg.AutoUpdate.Enabled && rawUpdateAvailable && dockerPublishReady,
		UpdaterAvailable:     updaterHealth.Available,
		UpdaterHealthStatus:  updaterHealth.Status,
		UpdaterHealthMessage: updaterHealth.Message,
	}
	if !resp.Enabled {
		resp.Message = "auto update disabled"
	} else if branchErr != nil || frontendErr != nil {
		resp.Message = BuildUpdateCheckWarning(branchErr, frontendErr)
	} else if !dockerPublishReady {
		resp.Message = dockerPublishMessage
	} else if !resp.UpdateAvailable {
		resp.Message = "already up to date"
	}
	return resp, nil
}

func (s *Service) BuildCurrentUpdateState(ctx context.Context) *CheckResponse {
	cfg := s.config()
	cfg.SanitizeAutoUpdate()

	channel := cfg.AutoUpdate.Channel
	if channel == "auto" {
		channel = InferAutoUpdateChannel(s.deps.CurrentVersion, os.Getenv(AutoUpdateChannelEnv))
	}

	currentUIVersion, currentUICommit := s.CurrentFrontendState()
	updaterHealth := CheckUpdaterHealth(ctx, cfg)

	return &CheckResponse{
		Enabled:              cfg.AutoUpdate.Enabled,
		CurrentVersion:       CurrentUpdateDisplayVersion(s.deps.CurrentVersion),
		CurrentCommit:        strings.TrimSpace(s.deps.CurrentCommit),
		CurrentUIVersion:     currentUIVersion,
		CurrentUICommit:      currentUICommit,
		BuildDate:            s.depsCurrentBuildDate(),
		TargetChannel:        channel,
		DockerImage:          cfg.AutoUpdate.DockerImage,
		DockerTag:            DockerTagForChannel(channel, ""),
		UpdaterAvailable:     updaterHealth.Available,
		UpdaterHealthStatus:  updaterHealth.Status,
		UpdaterHealthMessage: updaterHealth.Message,
	}
}

func (s *Service) CurrentFrontendState() (string, string) {
	version := s.deps.CurrentFrontendVersion
	ref := s.deps.CurrentFrontendRef
	commit := strings.TrimSpace(s.deps.CurrentFrontendCommit)

	if meta, ok := managementasset.CurrentPanelMetadata(s.deps.ConfigFilePath); ok {
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

	return CurrentFrontendDisplayVersion(version, ref, commit), strings.TrimSpace(commit)
}

func (s *Service) FetchProgress(ctx context.Context) (*ProgressResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, JoinURLPath(ResolveUpdaterURL(s.cfg), "/v1/status"), nil)
	if err != nil {
		return nil, err
	}
	if token := UpdaterToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: UpdateHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("updater status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var payload ProgressResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return &payload, nil
}

func (s *Service) OpenProgressStream(ctx context.Context, lastEventID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, JoinURLPath(ResolveUpdaterURL(s.cfg), "/v1/events"), nil)
	if err != nil {
		return nil, err
	}
	if token := UpdaterToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if id := strings.TrimSpace(lastEventID); id != "" {
		req.Header.Set("Last-Event-ID", id)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   UpdateStreamDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: UpdateStreamDialTimeout,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("updater events %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp, nil
}

func (s *Service) TriggerUpdate(ctx context.Context, check *CheckResponse) (*TriggerResponse, error) {
	if check == nil {
		return nil, fmt.Errorf("update target is nil")
	}
	payload := map[string]string{
		"image":                check.DockerImage,
		"tag":                  check.DockerTag,
		"channel":              check.TargetChannel,
		"current_version":      check.CurrentVersion,
		"current_commit":       check.CurrentCommit,
		"current_ui_version":   check.CurrentUIVersion,
		"current_ui_commit":    check.CurrentUICommit,
		"version":              check.LatestVersion,
		"commit":               check.LatestCommit,
		"commit_url":           check.LatestCommitURL,
		"ui_version":           check.LatestUIVersion,
		"ui_commit":            check.LatestUICommit,
		"ui_commit_url":        check.LatestUICommitURL,
		"release_name":         check.ReleaseName,
		"release_tag":          check.ReleaseTag,
		"release_notes":        check.ReleaseNotes,
		"release_url":          check.ReleaseURL,
		"release_published_at": check.ReleasePublishedAt,
		"service":              UpdaterTargetService(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal_failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, JoinURLPath(ResolveUpdaterURL(s.cfg), "/v1/update"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request_create_failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := UpdaterToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: UpdateHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater_unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("updater_failed: %s", strings.TrimSpace(string(data)))
	}
	var result TriggerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("updater_response_invalid: %w", err)
	}
	if result.RunID == 0 {
		return nil, fmt.Errorf("updater_response_invalid: missing run_id")
	}
	return &result, nil
}

func formatReleaseTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func (s *Service) config() *config.Config {
	if s == nil || s.cfg == nil {
		return &config.Config{}
	}
	return s.cfg
}

func (s *Service) depsCurrentBuildDate() string {
	return strings.TrimSpace(s.deps.BuildDate)
}

func (s *Service) githubClient() *http.Client {
	client := &http.Client{Timeout: UpdateHTTPTimeout}
	cfg := s.config()
	proxyURL := strings.TrimSpace(cfg.ProxyURL)
	if proxyURL != "" {
		util.SetProxy(&sdkconfig.SDKConfig{
			ProxyURL:           proxyURL,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
			CACert:             cfg.CACert,
		}, client)
	}
	return client
}

func (s *Service) fetchBranchCommit(ctx context.Context, client *http.Client, repo string, channel string) (BranchCommitInfo, error) {
	if s != nil && s.deps.FetchBranchCommit != nil {
		return s.deps.FetchBranchCommit(ctx, client, repo, channel)
	}
	return FetchBranchCommit(ctx, client, repo, channel)
}

func (s *Service) fetchLatestReleaseInfo(ctx context.Context, client *http.Client, repo string) (ReleaseInfo, error) {
	if s != nil && s.deps.FetchLatestReleaseInfo != nil {
		return s.deps.FetchLatestReleaseInfo(ctx, client, repo)
	}
	return FetchLatestReleaseInfo(ctx, client, repo)
}

func (s *Service) fetchSuccessfulWorkflowRun(ctx context.Context, client *http.Client, repo string, workflow string, branch string) (WorkflowRunInfo, error) {
	if s != nil && s.deps.FetchSuccessfulWorkflowRun != nil {
		return s.deps.FetchSuccessfulWorkflowRun(ctx, client, repo, workflow, branch)
	}
	return FetchLatestSuccessfulWorkflowRun(ctx, client, repo, workflow, branch)
}

func (s *Service) verifyDockerPublishReady(ctx context.Context, client *http.Client, repo string, channel string, backend BranchCommitInfo, frontend BranchCommitInfo) error {
	run, err := s.fetchSuccessfulWorkflowRun(ctx, client, repo, DockerPublishWorkflow, channel)
	if err != nil {
		return fmt.Errorf("docker image readiness check failed: %w", err)
	}
	if !SameCommit(run.HeadSHA, backend.SHA) {
		return fmt.Errorf(
			"docker image for %s is not ready; latest successful publish is %s but branch head is %s",
			channel,
			ShortCommit(run.HeadSHA),
			ShortCommit(backend.SHA),
		)
	}

	sourceTime := LatestCommitTime(backend, frontend)
	runTime := run.CreatedAt
	if runTime.IsZero() {
		runTime = run.UpdatedAt
	}
	if !sourceTime.IsZero() && !runTime.IsZero() && runTime.Before(sourceTime) {
		return fmt.Errorf("docker image for %s is not ready; latest successful publish predates the latest source commit", channel)
	}
	return nil
}
