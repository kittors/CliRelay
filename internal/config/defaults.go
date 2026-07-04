package config

const (
	DefaultPanelGitHubRepository      = "https://github.com/kittors/codeProxy"
	DefaultPprofAddr                  = "127.0.0.1:8316"
	DefaultAutoUpdateChannel          = "main"
	DefaultAutoUpdateRepository       = "https://github.com/kittors/CliRelay"
	DefaultAutoUpdateDockerImage      = "ghcr.io/kittors/clirelay"
	DefaultAutoUpdateUpdaterURL       = "http://clirelay-updater:8320"
	DefaultModelRequestBodyMB         = 128
	DefaultRequestBodyDiskThresholdMB = 8

	// EnvAuthPath overrides auth-dir with the path visible inside the running container/process.
	EnvAuthPath = "AUTH_PATH"
	// EnvPort overrides the configured listen port for blue-green deploy slots.
	EnvPort = "CLIRELAY_PORT"
	// EnvLegacyPort keeps the existing Docker installer PORT environment useful.
	EnvLegacyPort = "PORT"
)
