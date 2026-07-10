package updateflow

import "time"

const (
	UpdateHTTPTimeout       = 10 * time.Second
	UpdaterHealthTimeout    = 2 * time.Second
	UpdateStreamDialTimeout = 3 * time.Second
	UpdaterTokenEnv         = "CLIRELAY_UPDATER_TOKEN"
	GitHubTokenEnv          = "CLIRELAY_GITHUB_TOKEN"
	AutoUpdateChannelEnv    = "CLIRELAY_UPDATE_CHANNEL"
	DefaultUpdaterService   = "clirelay"
	DockerPublishWorkflow   = "docker-publish.yml"
	GitHubUserAgent         = "CLIProxyAPI"
)

const (
	UpdaterHealthStatusOK              = "ok"
	UpdaterHealthStatusTokenMissing    = "token_missing"
	UpdaterHealthStatusAuthFailed      = "auth_failed"
	UpdaterHealthStatusUnreachable     = "unreachable"
	UpdaterHealthStatusBadStatus       = "bad_status"
	UpdaterHealthStatusUpgradeRequired = "upgrade_required"
	UpdaterHealthStatusRequestInvalid  = "request_invalid"
)

type UpdaterHealth struct {
	Available bool
	Status    string
	Message   string
}

type CheckResponse struct {
	Enabled              bool   `json:"enabled"`
	CurrentVersion       string `json:"current_version"`
	CurrentCommit        string `json:"current_commit"`
	CurrentUIVersion     string `json:"current_ui_version,omitempty"`
	CurrentUICommit      string `json:"current_ui_commit,omitempty"`
	BuildDate            string `json:"build_date"`
	TargetChannel        string `json:"target_channel"`
	LatestVersion        string `json:"latest_version"`
	LatestCommit         string `json:"latest_commit"`
	LatestCommitURL      string `json:"latest_commit_url,omitempty"`
	LatestUIVersion      string `json:"latest_ui_version,omitempty"`
	LatestUICommit       string `json:"latest_ui_commit,omitempty"`
	LatestUICommitURL    string `json:"latest_ui_commit_url,omitempty"`
	DockerImage          string `json:"docker_image"`
	DockerTag            string `json:"docker_tag"`
	ReleaseName          string `json:"release_name,omitempty"`
	ReleaseTag           string `json:"release_tag,omitempty"`
	ReleaseNotes         string `json:"release_notes,omitempty"`
	ReleaseURL           string `json:"release_url,omitempty"`
	ReleasePublishedAt   string `json:"release_published_at,omitempty"`
	UpdateAvailable      bool   `json:"update_available"`
	UpdaterAvailable     bool   `json:"updater_available"`
	UpdaterHealthStatus  string `json:"updater_health_status,omitempty"`
	UpdaterHealthMessage string `json:"updater_health_message,omitempty"`
	Message              string `json:"message,omitempty"`
}

type ProgressLogEntry struct {
	Timestamp string `json:"timestamp"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

type ProgressResponse struct {
	RunID              uint64             `json:"run_id,omitempty"`
	EventID            uint64             `json:"event_id,omitempty"`
	Status             string             `json:"status"`
	Stage              string             `json:"stage"`
	MessageCode        string             `json:"message_code,omitempty"`
	Message            string             `json:"message,omitempty"`
	ProgressPercent    float64            `json:"progress_percent,omitempty"`
	ProgressCurrent    int                `json:"progress_current,omitempty"`
	ProgressTotal      int                `json:"progress_total,omitempty"`
	ProgressUnit       string             `json:"progress_unit,omitempty"`
	Service            string             `json:"service,omitempty"`
	CurrentVersion     string             `json:"current_version,omitempty"`
	CurrentCommit      string             `json:"current_commit,omitempty"`
	CurrentUIVersion   string             `json:"current_ui_version,omitempty"`
	CurrentUICommit    string             `json:"current_ui_commit,omitempty"`
	TargetImage        string             `json:"target_image,omitempty"`
	TargetTag          string             `json:"target_tag,omitempty"`
	TargetVersion      string             `json:"target_version,omitempty"`
	TargetCommit       string             `json:"target_commit,omitempty"`
	TargetCommitURL    string             `json:"target_commit_url,omitempty"`
	TargetUIVersion    string             `json:"target_ui_version,omitempty"`
	TargetUICommit     string             `json:"target_ui_commit,omitempty"`
	TargetUICommitURL  string             `json:"target_ui_commit_url,omitempty"`
	TargetChannel      string             `json:"target_channel,omitempty"`
	ReleaseName        string             `json:"release_name,omitempty"`
	ReleaseTag         string             `json:"release_tag,omitempty"`
	ReleaseNotes       string             `json:"release_notes,omitempty"`
	ReleaseURL         string             `json:"release_url,omitempty"`
	ReleasePublishedAt string             `json:"release_published_at,omitempty"`
	StartedAt          string             `json:"started_at,omitempty"`
	UpdatedAt          string             `json:"updated_at,omitempty"`
	FinishedAt         string             `json:"finished_at,omitempty"`
	Logs               []ProgressLogEntry `json:"logs,omitempty"`
}

type TriggerResponse struct {
	Status  string `json:"status"`
	Service string `json:"service,omitempty"`
	RunID   uint64 `json:"run_id"`
}
type GitCommitActor struct {
	Date time.Time `json:"date"`
}

type BranchCommitInfo struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message   string         `json:"message"`
		Author    GitCommitActor `json:"author"`
		Committer GitCommitActor `json:"committer"`
	} `json:"commit"`
}

type WorkflowRunInfo struct {
	ID         int64     `json:"id"`
	HTMLURL    string    `json:"html_url"`
	HeadSHA    string    `json:"head_sha"`
	HeadBranch string    `json:"head_branch"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Event      string    `json:"event"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type WorkflowRunsResponse struct {
	WorkflowRuns []WorkflowRunInfo `json:"workflow_runs"`
}

type ReleaseInfo struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
}
