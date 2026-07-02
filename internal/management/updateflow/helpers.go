package updateflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func InferAutoUpdateChannel(version string, envChannel string) string {
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

func CurrentUpdateDisplayVersion(version string) string {
	return strings.TrimSpace(version)
}

func LatestUpdateDisplayVersion(channel string, commit string) string {
	normalized := NormalizeAutoUpdateChannel(channel)
	if normalized == "dev" {
		return JoinChannelCommit("dev", commit)
	}
	return JoinChannelCommit("main", commit)
}

func CurrentFrontendDisplayVersion(version string, ref string, commit string) string {
	trimmed := strings.TrimSpace(version)
	if trimmed != "" && !strings.EqualFold(trimmed, "dev") {
		return trimmed
	}
	normalizedRef := NormalizeAutoUpdateChannel(ref)
	if normalizedRef == "auto" || normalizedRef == "" {
		normalizedRef = "main"
	}
	return LatestFrontendDisplayVersion(normalizedRef, commit)
}

func LatestFrontendDisplayVersion(channel string, commit string) string {
	normalized := NormalizeAutoUpdateChannel(channel)
	if normalized == "dev" {
		return "panel-" + JoinChannelCommit("dev", commit)
	}
	return "panel-" + JoinChannelCommit("main", commit)
}

func AutoUpdateAvailableFromCommit(currentCommit string, latestCommit string) bool {
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

func AutoUpdateAvailable(currentBackendCommit string, latestBackendCommit string, currentFrontendCommit string, latestFrontendCommit string) bool {
	return AutoUpdateAvailableFromCommit(currentBackendCommit, latestBackendCommit) ||
		AutoUpdateAvailableFromCommit(currentFrontendCommit, latestFrontendCommit)
}

func DockerTagForChannel(channel string, _ string) string {
	if strings.EqualFold(strings.TrimSpace(channel), "dev") {
		return "dev"
	}
	return "latest"
}

func NormalizeAutoUpdateChannel(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "main", "dev", "auto":
		return strings.ToLower(strings.TrimSpace(channel))
	default:
		return ""
	}
}

func NormalizeGitHubRepository(repo string) string {
	trimmed := strings.TrimSpace(repo)
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

func GitHubAPIURL(repo string, path string) string {
	return "https://api.github.com/repos/" + strings.Trim(repo, "/") + "/" + strings.TrimLeft(path, "/")
}

func ApplyGitHubAPIHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", GitHubUserAgent)
	if token := GitHubAPIToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func GitHubAPIToken() string {
	if token := strings.TrimSpace(os.Getenv(GitHubTokenEnv)); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

func ResolveUpdaterURL(cfg *config.Config) string {
	if fromEnv := strings.TrimSpace(os.Getenv("CLIRELAY_UPDATER_URL")); fromEnv != "" {
		return fromEnv
	}
	if cfg != nil && cfg.AutoUpdate.UpdaterURL != "" {
		return cfg.AutoUpdate.UpdaterURL
	}
	return config.DefaultAutoUpdateUpdaterURL
}

func UpdaterToken() string {
	return strings.TrimSpace(os.Getenv(UpdaterTokenEnv))
}

func UpdaterTargetService() string {
	if service := strings.TrimSpace(os.Getenv("CLIRELAY_TARGET_SERVICE")); service != "" {
		return service
	}
	return DefaultUpdaterService
}

func JoinURLPath(base string, path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if trimmed == "" {
		trimmed = config.DefaultAutoUpdateUpdaterURL
	}
	return trimmed + "/" + strings.TrimLeft(path, "/")
}

func CheckUpdaterAvailable(ctx context.Context, cfg *config.Config) bool {
	return CheckUpdaterHealth(ctx, cfg).Available
}

func CheckUpdaterHealth(ctx context.Context, cfg *config.Config) UpdaterHealth {
	token := UpdaterToken()
	if token == "" {
		return UpdaterHealth{
			Status:  UpdaterHealthStatusTokenMissing,
			Message: "updater token is not configured",
		}
	}

	ctx, cancel := context.WithTimeout(ctx, UpdaterHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, JoinURLPath(ResolveUpdaterURL(cfg), "/v1/health"), nil)
	if err != nil {
		return UpdaterHealth{
			Status:  UpdaterHealthStatusRequestInvalid,
			Message: "updater health request could not be created",
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return UpdaterHealth{
			Status:  UpdaterHealthStatusUnreachable,
			Message: "updater health request failed: " + err.Error(),
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return UpdaterHealth{Available: true, Status: UpdaterHealthStatusOK}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return UpdaterHealth{
			Status:  UpdaterHealthStatusAuthFailed,
			Message: "updater token is missing or does not match",
		}
	}
	return UpdaterHealth{
		Status:  UpdaterHealthStatusBadStatus,
		Message: fmt.Sprintf("updater health returned HTTP %d", resp.StatusCode),
	}
}

func FetchBranchCommit(ctx context.Context, client *http.Client, repo string, channel string) (BranchCommitInfo, error) {
	var info BranchCommitInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubAPIURL(repo, "commits/"+channel), nil)
	if err != nil {
		return info, err
	}
	ApplyGitHubAPIHeaders(req)
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

func FetchLatestReleaseInfo(ctx context.Context, client *http.Client, repo string) (ReleaseInfo, error) {
	var info ReleaseInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubAPIURL(repo, "releases/latest"), nil)
	if err != nil {
		return info, err
	}
	ApplyGitHubAPIHeaders(req)
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

func FetchLatestSuccessfulWorkflowRun(ctx context.Context, client *http.Client, repo string, workflow string, branch string) (WorkflowRunInfo, error) {
	var info WorkflowRunInfo
	endpoint := GitHubAPIURL(repo, "actions/workflows/"+url.PathEscape(strings.TrimSpace(workflow))+"/runs")
	query := url.Values{}
	query.Set("branch", strings.TrimSpace(branch))
	query.Set("status", "success")
	query.Set("per_page", "20")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return info, err
	}
	ApplyGitHubAPIHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return info, fmt.Errorf("github workflow runs status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var payload WorkflowRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return info, err
	}
	for _, run := range payload.WorkflowRuns {
		if strings.EqualFold(strings.TrimSpace(run.Status), "completed") &&
			strings.EqualFold(strings.TrimSpace(run.Conclusion), "success") {
			return run, nil
		}
	}
	return info, fmt.Errorf("no successful %s run found for %s", workflow, strings.TrimSpace(branch))
}

func BuildUpdateCheckWarning(branchErr error, frontendErr error) string {
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

func ShouldVerifyDockerPublish(cfg *config.Config, repo string) bool {
	if cfg == nil {
		return false
	}
	return NormalizeGitHubRepository(repo) == NormalizeGitHubRepository(config.DefaultAutoUpdateRepository) &&
		strings.EqualFold(strings.TrimSpace(cfg.AutoUpdate.DockerImage), config.DefaultAutoUpdateDockerImage)
}

func LatestCommitTime(commits ...BranchCommitInfo) time.Time {
	var latest time.Time
	for _, commit := range commits {
		candidate := commit.Commit.Committer.Date
		if candidate.IsZero() {
			candidate = commit.Commit.Author.Date
		}
		if candidate.After(latest) {
			latest = candidate
		}
	}
	return latest
}

func SameCommit(left string, right string) bool {
	a := strings.ToLower(strings.TrimSpace(left))
	b := strings.ToLower(strings.TrimSpace(right))
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func JoinChannelCommit(channel string, commit string) string {
	short := ShortCommit(commit)
	if short == "" {
		return channel
	}
	return channel + "-" + short
}

func ShortCommit(commit string) string {
	trimmed := strings.TrimSpace(commit)
	if len(trimmed) > 7 {
		return trimmed[:7]
	}
	return trimmed
}
