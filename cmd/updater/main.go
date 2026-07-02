package main

import (
	"bufio"
	"context"
	"crypto/subtle"
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
	"strings"
	"sync"
	"syscall"
	"time"
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

	if err := persistRequestedImage(s.envFile, req.Image, req.Tag); err != nil {
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

func persistRequestedImage(envFile string, image string, tag string) error {
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
		return fmt.Errorf("%w: missing configured CLI_PROXY_IMAGE", errRequestedImageNotAllowed)
	}
	if err != nil {
		return err
	}

	lines := splitEnvLines(string(data))
	configuredRepo := imageRepository(configuredImageRef(lines))
	requestedRepo := imageRepository(imageRef)
	if configuredRepo == "" {
		return fmt.Errorf("%w: missing configured CLI_PROXY_IMAGE", errRequestedImageNotAllowed)
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
	return os.WriteFile(envFile, []byte(content), 0o600)
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
	reporter.Stage("pulling", "pulling target image")
	if err := runDockerCompose(ctx, composeFile, envFile, projectName, reporter, "pull", service); err != nil {
		return err
	}
	reporter.Stage("restarting", "restarting service container")
	if err := runDockerCompose(ctx, composeFile, envFile, projectName, reporter, "up", "-d", "--remove-orphans", service); err != nil {
		return err
	}
	reporter.Stage("verifying", "docker update commands completed")
	return nil
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
