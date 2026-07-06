package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultListenAddr    = ":8320"
	defaultComposeFile   = ""
	defaultEnvFile       = ""
	defaultTargetService = "clirelay"
	updateCommandTimeout = 10 * time.Minute
	shutdownTimeout      = 10 * time.Second
	maxUpdateLogEntries  = 200
)

var errRequestedImageNotAllowed = errors.New("requested image is not allowed")

type composeRunner func(ctx context.Context, composeFile string, envFile string, projectName string, service string, reporter updateReporter) error

type updateReporter interface {
	Stage(stage string, message string)
	Log(stream string, message string)
}

type updaterConfig struct {
	Addr           string
	Token          string
	ComposeFile    string
	EnvFile        string
	ProjectName    string
	DefaultService string
	Runner         composeRunner
	Context        context.Context
}

type updaterServer struct {
	token          string
	composeFile    string
	envFile        string
	projectName    string
	defaultService string
	runner         composeRunner
	mu             sync.Mutex
	runID          uint64
	status         updateStatusResponse
	pullSkipped    bool
	pullSkipLog    string
	ctx            context.Context
}

type updateLogEntry struct {
	Timestamp string `json:"timestamp"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

type updateStatusResponse struct {
	Status          string           `json:"status"`
	Stage           string           `json:"stage"`
	Message         string           `json:"message,omitempty"`
	Service         string           `json:"service,omitempty"`
	TargetImage     string           `json:"target_image,omitempty"`
	TargetTag       string           `json:"target_tag,omitempty"`
	TargetVersion   string           `json:"target_version,omitempty"`
	TargetCommit    string           `json:"target_commit,omitempty"`
	TargetUIVersion string           `json:"target_ui_version,omitempty"`
	TargetUICommit  string           `json:"target_ui_commit,omitempty"`
	TargetChannel   string           `json:"target_channel,omitempty"`
	StartedAt       string           `json:"started_at,omitempty"`
	UpdatedAt       string           `json:"updated_at,omitempty"`
	FinishedAt      string           `json:"finished_at,omitempty"`
	Logs            []updateLogEntry `json:"logs,omitempty"`
}

type updateRequest struct {
	Service   string `json:"service"`
	Image     string `json:"image"`
	Tag       string `json:"tag"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	UIVersion string `json:"ui_version"`
	UICommit  string `json:"ui_commit"`
	Channel   string `json:"channel"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runUpdater(ctx, updaterConfigFromEnv())
}

func updaterConfigFromEnv() updaterConfig {
	return updaterConfig{
		Addr:           envOrDefault("CLIRELAY_UPDATER_ADDR", defaultListenAddr),
		Token:          strings.TrimSpace(os.Getenv("CLIRELAY_UPDATER_TOKEN")),
		ComposeFile:    envOrDefault("CLIRELAY_COMPOSE_FILE", defaultComposeFile),
		EnvFile:        envOrDefault("CLIRELAY_ENV_FILE", defaultEnvFile),
		ProjectName:    strings.TrimSpace(os.Getenv("CLIRELAY_COMPOSE_PROJECT_NAME")),
		DefaultService: envOrDefault("CLIRELAY_TARGET_SERVICE", defaultTargetService),
		Runner:         runComposeUpdate,
	}
}

func runUpdater(ctx context.Context, cfg updaterConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	addr := envOrDefaultValue(cfg.Addr, defaultListenAddr)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("clirelay updater: listen on %s failed: %w", addr, err)
	}
	return serveUpdater(ctx, cfg, listener)
}

func serveUpdater(ctx context.Context, cfg updaterConfig, listener net.Listener) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg.Context = ctx
	server := newUpdaterServer(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", server.handleHealth)
	mux.HandleFunc("/v1/status", server.handleStatus)
	mux.HandleFunc("/v1/update", server.handleUpdate)

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("clirelay updater listening on %s", listener.Addr())
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("clirelay updater: shutdown failed: %w", err)
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func newUpdaterServer(cfg updaterConfig) *updaterServer {
	runner := cfg.Runner
	if runner == nil {
		runner = runComposeUpdate
	}
	ctx := cfg.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return &updaterServer{
		token:          strings.TrimSpace(cfg.Token),
		composeFile:    envOrDefaultValue(cfg.ComposeFile, defaultComposeFile),
		envFile:        envOrDefaultValue(cfg.EnvFile, defaultEnvFile),
		projectName:    strings.TrimSpace(cfg.ProjectName),
		defaultService: envOrDefaultValue(cfg.DefaultService, defaultTargetService),
		runner:         runner,
		ctx:            ctx,
		status: updateStatusResponse{
			Status: "idle",
			Stage:  "idle",
		},
	}
}

func (s *updaterServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	snapshot := s.snapshot()
	payload := map[string]string{"status": snapshot.Status}
	if snapshot.Status == "failed" && strings.TrimSpace(snapshot.Message) != "" {
		payload["error"] = snapshot.Message
	} else {
		payload["error"] = ""
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *updaterServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (s *updaterServer) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req updateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	service := sanitizeServiceName(req.Service)
	if service == "" {
		service = s.defaultService
	}
	if service == "" {
		http.Error(w, "missing target service", http.StatusBadRequest)
		return
	}

	envFile := s.envFile
	if strings.TrimSpace(envFile) == "" && strings.TrimSpace(s.composeFile) != "" {
		envFile = filepath.Join(filepath.Dir(s.composeFile), ".env")
	}
	if err := persistRequestedImage(s.context(), envFile, req.Image, req.Tag); err != nil {
		if errors.Is(err, errRequestedImageNotAllowed) {
			message := err.Error()
			log.Print(message)
			s.setStatus("failed", message)
			http.Error(w, message, http.StatusBadRequest)
			return
		}
		message := "failed to update env file: " + err.Error()
		log.Print(message)
		s.setStatus("failed", message)
		http.Error(w, message, http.StatusInternalServerError)
		return
	}

	runID := s.startUpdate(service, req)
	go func() {
		ctx, cancel := context.WithTimeout(s.context(), updateCommandTimeout)
		defer cancel()
		reporter := updaterRunReporter{server: s, runID: runID}
		if err := s.runner(ctx, s.composeFile, s.envFile, s.projectName, service, reporter); err != nil {
			log.Printf("compose update failed: %v", err)
			reporter.Stage("failed", err.Error())
			s.finishUpdate(runID, "failed", "failed", err.Error())
			return
		}
		if message, skipped := s.pullSkipFailure(runID); skipped {
			reporter.Stage("failed", message)
			s.finishUpdate(runID, "failed", "failed", message)
			return
		}
		reporter.Stage("completed", "update completed")
		s.finishUpdate(runID, "completed", "completed", "update completed")
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "service": service})
}

func (s *updaterServer) context() context.Context {
	if s == nil || s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

func persistRequestedImage(ctx context.Context, envFile string, image string, tag string) error {
	imageRef := requestedImageRef(image, tag)
	if imageRef == "" {
		if strings.TrimSpace(image) == "" && strings.TrimSpace(tag) == "" {
			return nil
		}
		return fmt.Errorf("%w: invalid image or tag", errRequestedImageNotAllowed)
	}
	if strings.TrimSpace(envFile) == "" {
		return nil
	}

	data, err := os.ReadFile(envFile)
	if os.IsNotExist(err) {
		return writeDeploymentFile(ctx, envFile, []byte("CLI_PROXY_IMAGE="+imageRef+"\n"), 0o600, updaterRunReporter{})
	}
	if err != nil {
		return err
	}

	lines := splitEnvLines(string(data))
	configuredRepo := imageRepository(configuredImageRef(lines))
	requestedRepo := imageRepository(imageRef)
	if configuredRepo == "" {
		lines = append(lines, "CLI_PROXY_IMAGE="+imageRef)
		return writeDeploymentFile(ctx, envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600, updaterRunReporter{})
	}
	if requestedRepo != configuredRepo {
		return fmt.Errorf("%w: %s does not match %s", errRequestedImageNotAllowed, requestedRepo, configuredRepo)
	}

	line := "CLI_PROXY_IMAGE=" + imageRef
	replaced := false
	for i, existing := range lines {
		if strings.HasPrefix(existing, "CLI_PROXY_IMAGE=") {
			lines[i] = line
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, line)
	}
	content := strings.Join(lines, "\n") + "\n"
	return writeDeploymentFile(ctx, envFile, []byte(content), 0o600, updaterRunReporter{})
}

func configuredImageRef(lines []string) string {
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "CLI_PROXY_IMAGE" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

func imageRepository(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if digestIndex := strings.Index(ref, "@"); digestIndex >= 0 {
		return ref[:digestIndex]
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon]
	}
	return ref
}

func requestedImageRef(image string, tag string) string {
	cleanImage := strings.TrimSpace(image)
	cleanTag := strings.TrimSpace(tag)
	if cleanImage == "" || cleanTag == "" {
		return ""
	}
	if !isSafeImagePart(cleanImage) || !isSafeImagePart(cleanTag) {
		return ""
	}
	return fmt.Sprintf("%s:%s", cleanImage, cleanTag)
}

func splitEnvLines(content string) []string {
	trimmed := strings.TrimRight(content, "\r\n")
	if trimmed == "" {
		return nil
	}
	raw := strings.Split(trimmed, "\n")
	lines := raw[:0]
	for _, line := range raw {
		lines = append(lines, strings.TrimRight(line, "\r"))
	}
	return lines
}

func isSafeImagePart(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == '\'' || r == '"' || r == '\\' || r == '`' || r == '$' {
			return false
		}
	}
	return true
}

func (s *updaterServer) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = strings.TrimSpace(value[len("Bearer "):])
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(s.token)) == 1
}

func (s *updaterServer) startUpdate(service string, req updateRequest) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runID++
	now := time.Now().UTC().Format(time.RFC3339)
	s.status = updateStatusResponse{
		Status:          "running",
		Stage:           "preparing",
		Message:         "preparing update",
		Service:         service,
		TargetImage:     strings.TrimSpace(req.Image),
		TargetTag:       strings.TrimSpace(req.Tag),
		TargetVersion:   strings.TrimSpace(req.Version),
		TargetCommit:    strings.TrimSpace(req.Commit),
		TargetUIVersion: strings.TrimSpace(req.UIVersion),
		TargetUICommit:  strings.TrimSpace(req.UICommit),
		TargetChannel:   strings.TrimSpace(req.Channel),
		StartedAt:       now,
		UpdatedAt:       now,
		Logs:            nil,
	}
	s.pullSkipped = false
	s.pullSkipLog = ""
	return s.runID
}

func (s *updaterServer) setStatus(status string, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	s.status.Status = strings.TrimSpace(status)
	s.status.Stage = strings.TrimSpace(status)
	s.status.Message = strings.TrimSpace(message)
	s.status.UpdatedAt = now
	if status == "failed" || status == "completed" {
		s.status.FinishedAt = now
	}
}

func (s *updaterServer) appendLog(runID uint64, stream string, message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != s.runID {
		return
	}
	if s.status.Stage == "pulling" && strings.Contains(trimmed, "Skipped") {
		s.pullSkipped = true
		if s.pullSkipLog == "" {
			s.pullSkipLog = trimmed
		}
	}
	s.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.status.Logs = append(s.status.Logs, updateLogEntry{
		Timestamp: s.status.UpdatedAt,
		Stream:    strings.TrimSpace(stream),
		Message:   trimmed,
	})
	if len(s.status.Logs) > maxUpdateLogEntries {
		s.status.Logs = append([]updateLogEntry(nil), s.status.Logs[len(s.status.Logs)-maxUpdateLogEntries:]...)
	}
}

func (s *updaterServer) updateStage(runID uint64, stage string, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != s.runID {
		return
	}
	s.status.Stage = strings.TrimSpace(stage)
	s.status.Message = strings.TrimSpace(message)
	s.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

func (s *updaterServer) finishUpdate(runID uint64, status string, stage string, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != s.runID {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.status.Status = strings.TrimSpace(status)
	s.status.Stage = strings.TrimSpace(stage)
	s.status.Message = strings.TrimSpace(message)
	s.status.UpdatedAt = now
	s.status.FinishedAt = now
}

func (s *updaterServer) snapshot() updateStatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.status
	if len(s.status.Logs) > 0 {
		snapshot.Logs = append([]updateLogEntry(nil), s.status.Logs...)
	}
	return snapshot
}

func (s *updaterServer) pullSkipFailure(runID uint64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != s.runID || !s.pullSkipped {
		return "", false
	}
	message := "docker compose pull skipped the target service; check pull policy and image refresh settings"
	if strings.TrimSpace(s.pullSkipLog) != "" {
		message += ": " + strings.TrimSpace(s.pullSkipLog)
	}
	return message, true
}

type updaterRunReporter struct {
	server *updaterServer
	runID  uint64
}

func (r updaterRunReporter) Stage(stage string, message string) {
	if r.server == nil {
		return
	}
	r.server.updateStage(r.runID, stage, message)
}

func (r updaterRunReporter) Log(stream string, message string) {
	if r.server == nil {
		return
	}
	r.server.appendLog(r.runID, stream, message)
}

func runComposeUpdate(ctx context.Context, composeFile string, envFile string, projectName string, service string, reporter updateReporter) error {
	preparedEnvFile, err := ensureRuntimeDataStackConfig(ctx, composeFile, envFile, service, reporter)
	if err != nil {
		return err
	}
	envFile = preparedEnvFile
	reporter.Stage("pulling", "pulling target image")
	if err := runDockerCompose(ctx, composeFile, envFile, projectName, reporter, "pull", service); err != nil {
		return err
	}
	if composeFileHasService(composeFile, "clirelay-migrate") {
		reporter.Stage("migrating", "starting PostgreSQL/Redis before SQLite migration")
		if err := runDockerCompose(ctx, composeFile, envFile, projectName, reporter, "up", "-d", "postgres", "redis"); err != nil {
			return err
		}
		reporter.Stage("migrating", "migrating legacy SQLite data before restarting service")
		if err := runDockerCompose(ctx, composeFile, envFile, projectName, reporter, "run", "--rm", "clirelay-migrate"); err != nil {
			return err
		}
	}
	reporter.Stage("restarting", "restarting service container")
	if err := runDockerCompose(ctx, composeFile, envFile, projectName, reporter, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	reporter.Stage("verifying", "docker update commands completed")
	return nil
}

func ensureRuntimeDataStackConfig(ctx context.Context, composeFile string, envFile string, service string, reporter updateReporter) (string, error) {
	composeData, err := os.ReadFile(composeFile)
	if strings.TrimSpace(composeFile) == "" || os.IsNotExist(err) {
		return envFile, nil
	}
	if err != nil {
		return envFile, fmt.Errorf("read docker compose file: %w", err)
	}
	if strings.TrimSpace(envFile) == "" {
		envFile = filepath.Join(filepath.Dir(composeFile), ".env")
	}
	projectDir := deploymentProjectDir(ctx, composeFile)
	composeText := string(composeData)
	if hasComposeService(composeText, "postgres") && hasComposeService(composeText, "redis") && hasComposeService(composeText, "clirelay-init") && hasComposeService(composeText, "clirelay-migrate") {
		if err := ensureRuntimeEnvFile(ctx, envFile, projectDir, service, composeAppImage(composeText, service), reporter); err != nil {
			return envFile, err
		}
		return envFile, nil
	}

	reporter.Stage("preparing", "upgrading docker compose runtime data stack")
	nextCompose, appImage, err := upgradeComposeRuntimeStack(composeText, projectDir, service)
	if err != nil {
		return envFile, err
	}
	if err := ensureRuntimeEnvFile(ctx, envFile, projectDir, service, appImage, reporter); err != nil {
		return envFile, err
	}
	if err := writeDeploymentFile(ctx, composeFile, []byte(nextCompose), 0o644, reporter); err != nil {
		return envFile, fmt.Errorf("upgrade docker-compose.yml for PostgreSQL/Redis runtime stack: %w", err)
	}
	reporter.Log("stdout", "docker-compose.yml upgraded with PostgreSQL/Redis runtime services")
	return envFile, nil
}

func upgradeComposeRuntimeStack(composeText string, projectDir string, service string) (string, string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(composeText), &doc); err != nil {
		return "", "", fmt.Errorf("parse docker compose file: %w", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		return "", "", fmt.Errorf("docker compose file has no services section")
	}
	targetName := strings.TrimSpace(service)
	if _, ok := services[targetName]; !ok {
		targetName = firstApplicationService(services)
	}
	if targetName == "" {
		return "", "", fmt.Errorf("docker compose file has no CliRelay service to upgrade")
	}
	target, ok := stringMap(services[targetName])
	if !ok {
		target = map[string]any{}
		services[targetName] = target
	}
	appImage := stringValue(target["image"])
	if appImage == "" {
		appImage = "ghcr.io/kittors/clirelay:latest"
	}
	target["image"] = "${CLI_PROXY_IMAGE:-" + appImage + "}"
	target["entrypoint"] = sourceEnvEntrypoint()
	if !hasComposeCommand(target["command"]) {
		target["command"] = []any{"./CLIProxyAPI"}
	}
	targetEnv := withoutEnvKeys(target["environment"], runtimeStackEnvKeys()...)
	targetEnv["CLIRELAY_SQLITE_AUTO_MIGRATE"] = "false"
	target["environment"] = targetEnv
	target["volumes"] = appendVolume(target["volumes"], "${CLIRELAY_PROJECT_DIR:-"+projectDir+"}:/clirelay-deploy")
	targetNetworks := target["networks"]
	target["depends_on"] = map[string]any{
		"clirelay-init":    map[string]any{"condition": "service_completed_successfully"},
		"clirelay-migrate": map[string]any{"condition": "service_completed_successfully"},
		"postgres":         map[string]any{"condition": "service_healthy"},
		"redis":            map[string]any{"condition": "service_healthy"},
	}
	services["clirelay-init"] = initComposeService(projectDir, targetName, appImage)
	services["clirelay-migrate"] = migrateComposeService(target, appImage)
	services["postgres"] = postgresComposeService()
	services["redis"] = redisComposeService()
	services["clirelay-updater"] = updaterComposeService(projectDir, targetName, appImage)
	if targetNetworks != nil {
		for _, name := range []string{"clirelay-init", "clirelay-migrate", "postgres", "redis", "clirelay-updater"} {
			if generated, ok := stringMap(services[name]); ok {
				generated["networks"] = targetNetworks
			}
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", "", fmt.Errorf("render upgraded docker compose file: %w", err)
	}
	return string(out), appImage, nil
}

func composeAppImage(composeText string, service string) string {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(composeText), &doc); err != nil {
		return "ghcr.io/kittors/clirelay:latest"
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		return "ghcr.io/kittors/clirelay:latest"
	}
	targetName := strings.TrimSpace(service)
	if _, ok := services[targetName]; !ok {
		targetName = firstApplicationService(services)
	}
	if target, ok := stringMap(services[targetName]); ok {
		if image := stringValue(target["image"]); image != "" {
			return image
		}
	}
	return "ghcr.io/kittors/clirelay:latest"
}

func firstApplicationService(services map[string]any) string {
	for name := range services {
		if name != "postgres" && name != "redis" && name != "clirelay-init" && !strings.Contains(name, "updater") {
			return name
		}
	}
	return ""
}

func initComposeService(projectDir string, targetService string, image string) map[string]any {
	return map[string]any{
		"image":   "${CLI_PROXY_IMAGE:-" + image + "}",
		"command": []any{"clirelay-init-env"},
		"environment": map[string]any{
			"CLI_PROXY_IMAGE":               "${CLI_PROXY_IMAGE:-" + image + "}",
			"CLIRELAY_PROJECT_DIR":          "${CLIRELAY_PROJECT_DIR:-" + projectDir + "}",
			"CLIRELAY_ENV_FILE":             "/clirelay-deploy/.env",
			"CLIRELAY_COMPOSE_PROJECT_NAME": "${CLIRELAY_COMPOSE_PROJECT_NAME:-" + filepath.Base(projectDir) + "}",
			"CLIRELAY_TARGET_SERVICE":       "${CLIRELAY_TARGET_SERVICE:-" + targetService + "}",
		},
		"volumes": []any{"${CLIRELAY_PROJECT_DIR:-" + projectDir + "}:/clirelay-deploy"},
		"restart": "no",
	}
}

func postgresComposeService() map[string]any {
	return map[string]any{
		"image":      "postgres:15-alpine",
		"entrypoint": []any{"sh", "-c", "set -a; . /clirelay-deploy/.env; set +a; export POSTGRES_DB=\"$$CLIRELAY_POSTGRES_DB\" POSTGRES_USER=\"$$CLIRELAY_POSTGRES_USER\" POSTGRES_PASSWORD=\"$$CLIRELAY_POSTGRES_PASSWORD\"; exec docker-entrypoint.sh postgres"},
		"volumes": []any{
			"${CLIRELAY_POSTGRES_DATA_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/postgres-data}:/var/lib/postgresql/data",
			"${CLIRELAY_PROJECT_DIR:-${PWD:-.}}:/clirelay-deploy",
		},
		"healthcheck": map[string]any{
			"test":     []any{"CMD-SHELL", ". /clirelay-deploy/.env; pg_isready -U \"$$CLIRELAY_POSTGRES_USER\" -d \"$$CLIRELAY_POSTGRES_DB\""},
			"interval": "5s",
			"timeout":  "5s",
			"retries":  20,
		},
		"depends_on": map[string]any{
			"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
		},
		"restart": "unless-stopped",
	}
}

func redisComposeService() map[string]any {
	return map[string]any{
		"image":   "redis:7-alpine",
		"command": []any{"redis-server", "--appendonly", "yes"},
		"volumes": []any{"${CLIRELAY_REDIS_DATA_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/redis-data}:/data"},
		"healthcheck": map[string]any{
			"test":     []any{"CMD", "redis-cli", "ping"},
			"interval": "5s",
			"timeout":  "5s",
			"retries":  20,
		},
		"depends_on": map[string]any{
			"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
		},
		"restart": "unless-stopped",
	}
}

func updaterComposeService(projectDir string, targetService string, image string) map[string]any {
	return map[string]any{
		"image":      "${CLI_PROXY_IMAGE:-" + image + "}",
		"command":    []any{"./clirelay-updater"},
		"entrypoint": []any{"sh", "-c", "set -a; . /clirelay-deploy/.env; set +a; exec docker-entrypoint.sh ./clirelay-updater"},
		"user":       "0:0",
		"environment": map[string]any{
			"CLIRELAY_PROJECT_DIR":          "${CLIRELAY_PROJECT_DIR:-" + projectDir + "}",
			"CLIRELAY_COMPOSE_FILE":         "${CLIRELAY_PROJECT_DIR:-" + projectDir + "}/docker-compose.yml",
			"CLIRELAY_ENV_FILE":             "${CLIRELAY_ENV_FILE:-${CLIRELAY_PROJECT_DIR:-" + projectDir + "}/.env}",
			"CLIRELAY_COMPOSE_PROJECT_NAME": "${CLIRELAY_COMPOSE_PROJECT_NAME:-}",
			"CLIRELAY_TARGET_SERVICE":       "${CLIRELAY_TARGET_SERVICE:-" + targetService + "}",
		},
		"volumes": []any{
			"/var/run/docker.sock:/var/run/docker.sock",
			"${CLIRELAY_PROJECT_DIR:-" + projectDir + "}:${CLIRELAY_PROJECT_DIR:-" + projectDir + "}",
			"${CLIRELAY_PROJECT_DIR:-" + projectDir + "}:/clirelay-deploy",
		},
		"depends_on": map[string]any{
			"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
		},
		"restart": "unless-stopped",
	}
}

func sourceEnvEntrypoint() []any {
	return []any{"sh", "-c", "set -a; . /clirelay-deploy/.env; set +a; exec docker-entrypoint.sh \"$@\"", "--"}
}

func hasComposeCommand(value any) bool {
	if strings.TrimSpace(stringValue(value)) != "" {
		return true
	}
	if items, ok := value.([]any); ok {
		return len(items) > 0
	}
	if items, ok := value.([]string); ok {
		return len(items) > 0
	}
	return false
}

func deploymentProjectDir(ctx context.Context, composeFile string) string {
	projectDir := filepath.Dir(composeFile)
	if !strings.HasPrefix(filepath.Clean(composeFile), "/workspace"+string(os.PathSeparator)) {
		return projectDir
	}
	if hostDir, ok := hostProjectDirForMountedPath(ctx, composeFile); ok {
		return hostDir
	}
	return projectDir
}

func hostProjectDirForMountedPath(ctx context.Context, path string) (string, bool) {
	containerID, err := os.Hostname()
	if err != nil || strings.TrimSpace(containerID) == "" {
		return "", false
	}
	mountsJSON, err := dockerInspect(ctx, containerID, "{{json .Mounts}}")
	if err != nil {
		return "", false
	}
	var mounts []dockerMount
	if err := json.Unmarshal([]byte(mountsJSON), &mounts); err != nil {
		return "", false
	}
	return projectDirFromMounts(path, mounts)
}

func projectDirFromMounts(path string, mounts []dockerMount) (string, bool) {
	source, rel, dirMount, ok := hostPathForMountedPath(path, mounts)
	if !ok {
		return "", false
	}
	if !dirMount {
		return filepath.Dir(source), true
	}
	return filepath.Dir(filepath.Join(source, rel)), true
}

func runtimeStackEnvKeys() []string {
	return []string{
		"CLIRELAY_POSTGRES_DSN",
		"CLIRELAY_REDIS_ENABLE",
		"CLIRELAY_REDIS_ADDR",
		"CLIRELAY_REDIS_PASSWORD",
		"CLIRELAY_REDIS_DB",
		"CLIRELAY_TARGET_SERVICE",
		"CLIRELAY_UPDATER_URL",
		"CLIRELAY_UPDATER_TOKEN",
	}
}

func withoutEnvKeys(existing any, keys ...string) map[string]any {
	env := mergeEnv(existing, nil)
	for _, key := range keys {
		delete(env, key)
	}
	return env
}

func mergeEnv(existing any, values map[string]any) map[string]any {
	merged := map[string]any{}
	if current, ok := stringMap(existing); ok {
		for k, v := range current {
			merged[k] = v
		}
	} else if current, ok := existing.([]any); ok {
		for _, item := range current {
			key, value, ok := strings.Cut(stringValue(item), "=")
			if ok && strings.TrimSpace(key) != "" {
				merged[strings.TrimSpace(key)] = value
			}
		}
	}
	for k, v := range values {
		merged[k] = v
	}
	return merged
}

func appendVolume(existing any, volume string) []any {
	var volumes []any
	if current, ok := existing.([]any); ok {
		volumes = append(volumes, current...)
	} else if current, ok := existing.([]string); ok {
		for _, item := range current {
			volumes = append(volumes, item)
		}
	}
	for _, item := range volumes {
		if stringValue(item) == volume {
			return volumes
		}
	}
	return append(volumes, volume)
}

func stringMap(value any) (map[string]any, bool) {
	out, ok := value.(map[string]any)
	return out, ok
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func ensureRuntimeEnvFile(ctx context.Context, envFile string, projectDir string, service string, image string, reporter updateReporter) error {
	data, err := os.ReadFile(envFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read docker env file: %w", err)
	}
	lines := splitEnvLines(string(data))
	values := envValues(lines)
	setEnvDefault(&lines, values, "CLI_PROXY_IMAGE", image)
	setEnvDefaultOrReplaceWorkspace(&lines, values, "CLIRELAY_PROJECT_DIR", projectDir)
	setEnvDefault(&lines, values, "CLIRELAY_TARGET_SERVICE", service)
	setEnvDefault(&lines, values, "CLIRELAY_COMPOSE_PROJECT_NAME", filepath.Base(projectDir))
	setEnvDefault(&lines, values, "CLIRELAY_UPDATER_TOKEN", envOrDefault("CLIRELAY_UPDATER_TOKEN", randomHex(16)))
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_DB", "cliproxy")
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_USER", "cliproxy")
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_PASSWORD", randomHex(16))
	db := envOrDefaultValue(values["CLIRELAY_POSTGRES_DB"], "cliproxy")
	user := envOrDefaultValue(values["CLIRELAY_POSTGRES_USER"], "cliproxy")
	pass := envOrDefaultValue(values["CLIRELAY_POSTGRES_PASSWORD"], "cliproxy")
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_DSN", "postgres://"+user+":"+pass+"@postgres:5432/"+db+"?sslmode=disable")
	setEnvDefaultOrReplaceWorkspace(&lines, values, "CLIRELAY_POSTGRES_DATA_PATH", filepath.Join(projectDir, "postgres-data"))
	setEnvDefault(&lines, values, "CLIRELAY_REDIS_ENABLE", "true")
	setEnvDefault(&lines, values, "CLIRELAY_REDIS_ADDR", "redis:6379")
	setEnvDefault(&lines, values, "CLIRELAY_REDIS_DB", "0")
	setEnvDefaultOrReplaceWorkspace(&lines, values, "CLIRELAY_REDIS_DATA_PATH", filepath.Join(projectDir, "redis-data"))
	content := strings.Join(lines, "\n") + "\n"
	if err := writeDeploymentFile(ctx, envFile, []byte(content), 0o600, reporter); err != nil {
		return fmt.Errorf("write docker env file: %w", err)
	}
	return nil
}

func envValues(lines []string) map[string]string {
	values := map[string]string{}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return values
}

func setEnvDefault(lines *[]string, values map[string]string, key string, value string) {
	if strings.TrimSpace(values[key]) != "" {
		return
	}
	*lines = append(*lines, key+"="+value)
	values[key] = value
}

func setEnvDefaultOrReplaceWorkspace(lines *[]string, values map[string]string, key string, value string) {
	if existing := strings.TrimSpace(values[key]); existing != "" && !isWorkspacePath(existing) {
		return
	}
	for i, line := range *lines {
		currentKey, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(currentKey) == key {
			(*lines)[i] = key + "=" + value
			values[key] = value
			return
		}
	}
	*lines = append(*lines, key+"="+value)
	values[key] = value
}

func isWorkspacePath(value string) bool {
	clean := filepath.Clean(value)
	return clean == "/workspace" || strings.HasPrefix(clean, "/workspace"+string(os.PathSeparator))
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func writeDeploymentFile(ctx context.Context, path string, data []byte, mode os.FileMode, reporter updateReporter) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, mode); err == nil {
		return nil
	} else if fallbackErr := writeDeploymentFileViaDocker(ctx, path, data, mode, reporter); fallbackErr != nil {
		return fmt.Errorf("%v; docker fallback failed: %w", err, fallbackErr)
	}
	return nil
}

func writeDeploymentFileViaDocker(ctx context.Context, path string, data []byte, mode os.FileMode, reporter updateReporter) error {
	containerID, err := os.Hostname()
	if err != nil || strings.TrimSpace(containerID) == "" {
		return fmt.Errorf("detect updater container id: %w", err)
	}
	image, err := dockerInspect(ctx, containerID, "{{.Config.Image}}")
	if err != nil {
		return err
	}
	mountsJSON, err := dockerInspect(ctx, containerID, "{{json .Mounts}}")
	if err != nil {
		return err
	}
	var mounts []dockerMount
	if err := json.Unmarshal([]byte(mountsJSON), &mounts); err != nil {
		return fmt.Errorf("parse updater container mounts: %w", err)
	}
	source, rel, dirMount, ok := hostPathForMountedPath(path, mounts)
	if !ok {
		return fmt.Errorf("no writable host mount found for %s", path)
	}
	reporter.Log("stdout", "direct write failed; updating deployment file through docker mount fallback")
	modeText := fmt.Sprintf("%#o", mode.Perm())
	var cmd *exec.Cmd
	if dirMount {
		script := `set -eu; target="/host/$TARGET_REL"; mkdir -p "$(dirname "$target")"; cat > "$target"; chmod "$TARGET_MODE" "$target"`
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "-e", "TARGET_REL="+rel, "-e", "TARGET_MODE="+modeText, "-v", source+":/host", strings.TrimSpace(image), "sh", "-c", script)
	} else {
		script := `set -eu; cat > /target; chmod "$TARGET_MODE" /target`
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "-e", "TARGET_MODE="+modeText, "-v", source+":/target", strings.TrimSpace(image), "sh", "-c", script)
	}
	cmd.Stdin = strings.NewReader(string(data))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker helper write failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func dockerInspect(ctx context.Context, containerID string, format string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", format, containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker inspect updater container failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func hostPathForMountedPath(path string, mounts []dockerMount) (string, string, bool, bool) {
	cleanPath := filepath.Clean(path)
	var best dockerMount
	bestLen := -1
	for _, mount := range mounts {
		if strings.TrimSpace(mount.Source) == "" || strings.TrimSpace(mount.Destination) == "" {
			continue
		}
		dest := filepath.Clean(mount.Destination)
		if cleanPath == dest {
			return mount.Source, "", false, true
		}
		if strings.HasPrefix(cleanPath, dest+string(os.PathSeparator)) && len(dest) > bestLen {
			best = mount
			bestLen = len(dest)
		}
	}
	if bestLen < 0 {
		return "", "", false, false
	}
	rel, err := filepath.Rel(filepath.Clean(best.Destination), cleanPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", false, false
	}
	return best.Source, rel, true, true
}

func hasComposeService(content string, service string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == service+":" && len(line) > len(strings.TrimLeft(line, " \t")) {
			return true
		}
	}
	return false
}

func runDockerCompose(ctx context.Context, composeFile string, envFile string, projectName string, reporter updateReporter, args ...string) error {
	base := buildComposeArgs(composeFile, envFile, projectName, args...)
	cmd := exec.CommandContext(ctx, "docker", base...)
	reporter.Log("stdout", "$ docker "+strings.Join(base, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamCommandLogs(stdout, "stdout", reporter, &wg)
	go streamCommandLogs(stderr, "stderr", reporter, &wg)

	waitErr := cmd.Wait()
	wg.Wait()
	if waitErr != nil {
		return fmt.Errorf("docker compose %s failed: %w", strings.Join(args, " "), waitErr)
	}
	return nil
}

func streamCommandLogs(reader io.Reader, stream string, reporter updateReporter, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		reporter.Log(stream, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		reporter.Log("stderr", "log stream error: "+err.Error())
	}
}

func buildComposeArgs(composeFile string, envFile string, projectName string, args ...string) []string {
	base := []string{"compose"}
	if strings.TrimSpace(projectName) != "" {
		base = append(base, "--project-name", projectName)
	}
	if strings.TrimSpace(envFile) != "" {
		base = append(base, "--env-file", envFile)
	}
	if strings.TrimSpace(composeFile) != "" {
		base = append(base, "-f", composeFile)
	}
	base = append(base, args...)
	return base
}

func sanitizeServiceName(service string) string {
	trimmed := strings.TrimSpace(service)
	if trimmed == "" {
		return ""
	}
	for _, r := range trimmed {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return ""
		}
	}
	return trimmed
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func envOrDefault(key string, fallback string) string {
	return envOrDefaultValue(os.Getenv(key), fallback)
}

func envOrDefaultValue(value string, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}
