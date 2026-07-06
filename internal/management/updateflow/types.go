package updateflow

import "time"

const (
	UpdateHTTPTimeout     = 10 * time.Second
	UpdaterHealthTimeout  = 2 * time.Second
	UpdaterTokenEnv       = "CLIRELAY_UPDATER_TOKEN"
	GitHubTokenEnv        = "CLIRELAY_GITHUB_TOKEN"
	AutoUpdateChannelEnv  = "CLIRELAY_UPDATE_CHANNEL"
	DefaultUpdaterService = "clirelay"
	DockerPublishWorkflow = "docker-publish.yml"
	GitHubUserAgent       = "CLIProxyAPI"
)

const (
	UpdaterHealthStatusOK             = "ok"
	UpdaterHealthStatusTokenMissing   = "token_missing"
	UpdaterHealthStatusAuthFailed     = "auth_failed"
	UpdaterHealthStatusUnreachable    = "unreachable"
	UpdaterHealthStatusBadStatus      = "bad_status"
	UpdaterHealthStatusRequestInvalid = "request_invalid"
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
	ReleaseNotes         string `json:"release_notes,omitempty"`
	ReleaseURL           string `json:"release_url,omitempty"`
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
	Status          string             `json:"status"`
	Stage           string             `json:"stage"`
	Message         string             `json:"message,omitempty"`
	ProgressPercent float64            `json:"progress_percent,omitempty"`
	Migration       *MigrationProgress `json:"migration,omitempty"`
	Service         string             `json:"service,omitempty"`
	TargetImage     string             `json:"target_image,omitempty"`
	TargetTag       string             `json:"target_tag,omitempty"`
	TargetVersion   string             `json:"target_version,omitempty"`
	TargetCommit    string             `json:"target_commit,omitempty"`
	TargetUIVersion string             `json:"target_ui_version,omitempty"`
	TargetUICommit  string             `json:"target_ui_commit,omitempty"`
	TargetChannel   string             `json:"target_channel,omitempty"`
	StartedAt       string             `json:"started_at,omitempty"`
	UpdatedAt       string             `json:"updated_at,omitempty"`
	FinishedAt      string             `json:"finished_at,omitempty"`
	Logs            []ProgressLogEntry `json:"logs,omitempty"`
}

type MigrationProgress struct {
	Phase          string `json:"phase,omitempty"`
	TargetDatabase string `json:"target_database,omitempty"`
	SkipReason     string `json:"skip_reason,omitempty"`
	Table          string `json:"table,omitempty"`
	TableIndex     int    `json:"table_index,omitempty"`
	TableTotal     int    `json:"table_total,omitempty"`
	InsertedRows   int64  `json:"inserted_rows,omitempty"`
	TargetRows     int64  `json:"target_rows,omitempty"`
	PlannedInserts int64  `json:"planned_inserts,omitempty"`
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
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}
