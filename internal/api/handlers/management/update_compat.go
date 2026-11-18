package management

import (
	"context"
	"net/http"

	managementupdate "github.com/router-for-me/CLIProxyAPI/v8/internal/management/updateflow"
)

type updateCheckResponse = managementupdate.CheckResponse
type updateProgressLogEntry = managementupdate.ProgressLogEntry
type updateProgressResponse = managementupdate.ProgressResponse
type branchCommitInfo = managementupdate.BranchCommitInfo
type gitCommitActor = managementupdate.GitCommitActor
type workflowRunInfo = managementupdate.WorkflowRunInfo
type workflowRunsResponse = managementupdate.WorkflowRunsResponse

const (
	updateHTTPTimeout    = managementupdate.UpdateHTTPTimeout
	updaterHealthTimeout = managementupdate.UpdaterHealthTimeout
	updaterTokenEnv      = managementupdate.UpdaterTokenEnv
	githubTokenEnv       = managementupdate.GitHubTokenEnv
	autoUpdateChannelEnv = managementupdate.AutoUpdateChannelEnv

	defaultUpdaterService = managementupdate.DefaultUpdaterService
	dockerPublishWorkflow = managementupdate.DockerPublishWorkflow
)

var (
	fetchBranchCommitForUpdateCheck      = managementupdate.FetchBranchCommit
	fetchLatestReleaseInfoForUpdateCheck = func(ctx context.Context, client *http.Client, repo string) (releaseInfo, error) {
		info, err := managementupdate.FetchLatestReleaseInfo(ctx, client, repo)
		if err != nil {
			return releaseInfo{}, err
		}
		return releaseInfo{
			TagName:     info.TagName,
			Name:        info.Name,
			Body:        info.Body,
			HTMLURL:     info.HTMLURL,
			PublishedAt: info.PublishedAt,
		}, nil
	}
	fetchLatestSuccessfulWorkflowRunForUpdateCheck = managementupdate.FetchLatestSuccessfulWorkflowRun
)

func inferAutoUpdateChannel(version string, envChannel string) string {
	return managementupdate.InferAutoUpdateChannel(version, envChannel)
}

func currentUpdateDisplayVersion(version string) string {
	return managementupdate.CurrentUpdateDisplayVersion(version)
}

func latestUpdateDisplayVersion(channel string, commit string) string {
	return managementupdate.LatestUpdateDisplayVersion(channel, commit)
}

func currentFrontendDisplayVersion(version string, ref string, commit string) string {
	return managementupdate.CurrentFrontendDisplayVersion(version, ref, commit)
}

func latestFrontendDisplayVersion(channel string, commit string) string {
	return managementupdate.LatestFrontendDisplayVersion(channel, commit)
}

func autoUpdateAvailableFromCommit(currentCommit string, latestCommit string) bool {
	return managementupdate.AutoUpdateAvailableFromCommit(currentCommit, latestCommit)
}

func autoUpdateAvailable(currentBackendCommit string, latestBackendCommit string, currentFrontendCommit string, latestFrontendCommit string) bool {
	return managementupdate.AutoUpdateAvailable(currentBackendCommit, latestBackendCommit, currentFrontendCommit, latestFrontendCommit)
}

func dockerTagForChannel(channel string, commit string) string {
	return managementupdate.DockerTagForChannel(channel, commit)
}

func shortCommit(commit string) string {
	return managementupdate.ShortCommit(commit)
}

func fetchBranchCommit(ctx context.Context, client *http.Client, repo string, channel string) (branchCommitInfo, error) {
	return managementupdate.FetchBranchCommit(ctx, client, repo, channel)
}
