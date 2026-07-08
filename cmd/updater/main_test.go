package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func setUpdaterAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer secret")
}

func TestUpdaterRejectsInvalidBearerToken(t *testing.T) {
	server := newUpdaterServer(updaterConfig{
		Token: "secret",
		Runner: func(context.Context, string, string, string, string, updateReporter) error {
			t.Fatal("runner should not be called")
			return nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/update", strings.NewReader(`{"service":"clirelay"}`))
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestUpdaterRejectsRequestsWhenConfiguredTokenIsEmpty(t *testing.T) {
	server := newUpdaterServer(updaterConfig{
		Runner: func(context.Context, string, string, string, string, updateReporter) error {
			return nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/update", strings.NewReader(`{"service":"clirelay"}`))
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestServeUpdaterStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- serveUpdater(ctx, updaterConfig{Token: "secret"}, listener)
	}()

	waitForUpdaterHealth(t, listener.Addr().String())
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveUpdater returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for updater shutdown")
	}
}

func TestUpdaterCancelsRunnerOnServerContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	server := newUpdaterServer(updaterConfig{
		Token:   "secret",
		Context: ctx,
		Runner: func(ctx context.Context, _ string, _ string, _ string, _ string, _ updateReporter) error {
			close(started)
			<-ctx.Done()
			done <- ctx.Err()
			return ctx.Err()
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/update", strings.NewReader(`{"service":"clirelay"}`))
	setUpdaterAuth(req)
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner start")
	}

	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("runner context error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}
}

func TestUpdaterDefaultPathsDoNotPointAtWorkspace(t *testing.T) {
	server := newUpdaterServer(updaterConfig{})

	if strings.Contains(server.composeFile, "/workspace") {
		t.Fatalf("composeFile = %q, want no /workspace default", server.composeFile)
	}
	if strings.Contains(server.envFile, "/workspace") {
		t.Fatalf("envFile = %q, want no /workspace default", server.envFile)
	}
}

func TestUpdaterPersistsRequestedImageBeforeComposeUpdate(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:dev\nOTHER=value\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	called := make(chan struct{}, 1)
	server := newUpdaterServer(updaterConfig{
		Token:   "secret",
		EnvFile: envFile,
		Runner: func(_ context.Context, _ string, _ string, _ string, _ string, reporter updateReporter) error {
			data, err := os.ReadFile(envFile)
			if err != nil {
				t.Errorf("read env file: %v", err)
			}
			content := string(data)
			if !strings.Contains(content, "CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:latest\n") {
				t.Errorf("env file content = %q, want requested latest image persisted", content)
			}
			if !strings.Contains(content, "OTHER=value\n") {
				t.Errorf("env file content = %q, want unrelated values preserved", content)
			}
			reporter.Stage("pulling", "pulling image")
			reporter.Log("stdout", "docker compose pull cli-proxy-api")
			called <- struct{}{}
			return nil
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/update",
		strings.NewReader(`{"service":"cli-proxy-api","image":"ghcr.io/kittors/clirelay","tag":"latest"}`),
	)
	setUpdaterAuth(req)
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner")
	}
}

func TestUpdaterRestoresRequestedImageAfterRunnerFailure(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:latest\nOTHER=value\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	called := make(chan struct{}, 1)
	server := newUpdaterServer(updaterConfig{
		Token:   "secret",
		EnvFile: envFile,
		Runner: func(_ context.Context, _ string, _ string, _ string, _ string, _ updateReporter) error {
			called <- struct{}{}
			return errors.New("compose failed")
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/update",
		strings.NewReader(`{"service":"cli-proxy-api","image":"ghcr.io/kittors/clirelay","tag":"dev"}`),
	)
	setUpdaterAuth(req)
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner")
	}
	eventually(t, time.Second, func() bool {
		return server.snapshot().Status == "failed"
	})

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:latest\n") || strings.Contains(content, "CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:dev\n") {
		t.Fatalf("env file content = %q, want previous image restored", content)
	}
	if !strings.Contains(content, "OTHER=value\n") {
		t.Fatalf("env file content = %q, want unrelated values preserved", content)
	}
}

func TestPersistRequestedImageCreatesMissingEnvFile(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")

	if err := persistRequestedImage(context.Background(), envFile, "ghcr.io/kittors/clirelay", "latest"); err != nil {
		t.Fatalf("persistRequestedImage failed: %v", err)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if string(data) != "CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:latest\n" {
		t.Fatalf("env file content = %q, want requested image", string(data))
	}
}

func TestPersistRequestedImageAddsMissingImageEntry(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("OTHER=value\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	if err := persistRequestedImage(context.Background(), envFile, "ghcr.io/kittors/clirelay", "latest"); err != nil {
		t.Fatalf("persistRequestedImage failed: %v", err)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "OTHER=value\n") || !strings.Contains(content, "CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:latest\n") {
		t.Fatalf("env file content = %q, want existing value and requested image", content)
	}
}

func TestUpdaterRejectsRequestWhenEnvFileCannotBeUpdated(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.Mkdir(envFile, 0o700); err != nil {
		t.Fatalf("make env path directory: %v", err)
	}

	server := newUpdaterServer(updaterConfig{
		Token:   "secret",
		EnvFile: envFile,
		Runner: func(_ context.Context, _ string, _ string, _ string, _ string, _ updateReporter) error {
			t.Fatal("runner should not be called when env file cannot be updated")
			return nil
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/update",
		strings.NewReader(`{"service":"cli-proxy-api","image":"ghcr.io/kittors/clirelay","tag":"dev"}`),
	)
	setUpdaterAuth(req)
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to update env file") {
		t.Fatalf("body = %q, want env update failure", rec.Body.String())
	}
}

func TestUpdaterRejectsRequestedImageRepositoryChange(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:dev\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	called := make(chan struct{}, 1)
	server := newUpdaterServer(updaterConfig{
		Token:   "secret",
		EnvFile: envFile,
		Runner: func(_ context.Context, _ string, _ string, _ string, _ string, _ updateReporter) error {
			called <- struct{}{}
			return nil
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/update",
		strings.NewReader(`{"service":"cli-proxy-api","image":"ghcr.io/attacker/clirelay","tag":"latest"}`),
	)
	setUpdaterAuth(req)
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if string(data) != "CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:dev\n" {
		t.Fatalf("env file content = %q, want unchanged", string(data))
	}
	select {
	case <-called:
		t.Fatal("runner should not be called")
	default:
	}
}

func TestUpdaterAcceptsRequestAndRunsComposeUpdate(t *testing.T) {
	called := make(chan string, 1)
	server := newUpdaterServer(updaterConfig{
		Token:          "secret",
		ComposeFile:    "/workspace/docker-compose.yml",
		EnvFile:        "/workspace/.env",
		ProjectName:    "cliproxy",
		DefaultService: "clirelay",
		Runner: func(_ context.Context, composeFile string, envFile string, projectName string, service string, _ updateReporter) error {
			if composeFile != "/workspace/docker-compose.yml" {
				t.Errorf("composeFile = %q", composeFile)
			}
			if envFile != "/workspace/.env" {
				t.Errorf("envFile = %q", envFile)
			}
			if projectName != "cliproxy" {
				t.Errorf("projectName = %q", projectName)
			}
			called <- service
			return nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/update", strings.NewReader(`{"service":"cli-proxy-api"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	select {
	case service := <-called:
		if service != "cli-proxy-api" {
			t.Fatalf("service = %q, want cli-proxy-api", service)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner")
	}
}

func TestUpdaterStatusExposesTargetStageAndLogs(t *testing.T) {
	called := make(chan struct{}, 1)
	releaseRunner := make(chan struct{})
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:old\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	server := newUpdaterServer(updaterConfig{
		Token:   "secret",
		EnvFile: envFile,
		Runner: func(_ context.Context, _ string, _ string, _ string, service string, reporter updateReporter) error {
			reporter.Stage("pulling", "pulling image")
			reporter.Log("stdout", "docker compose pull "+service)
			called <- struct{}{}
			<-releaseRunner
			reporter.Stage("restarting", "restarting container")
			reporter.Log("stderr", "Container clirelay Recreated")
			return nil
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/update",
		strings.NewReader(`{"service":"clirelay","image":"ghcr.io/kittors/clirelay","tag":"dev","version":"dev-abcdef1","commit":"abcdef123456","channel":"dev"}`),
	)
	setUpdaterAuth(req)
	rec := httptest.NewRecorder()
	server.handleUpdate(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner")
	}

	statusRec := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	setUpdaterAuth(statusReq)
	server.handleStatus(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status endpoint code = %d, body=%s", statusRec.Code, statusRec.Body.String())
	}

	var payload updateStatusResponse
	if err := json.Unmarshal(statusRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if payload.Status != "running" {
		t.Fatalf("Status = %q, want running", payload.Status)
	}
	if payload.Stage != "pulling" {
		t.Fatalf("Stage = %q, want pulling", payload.Stage)
	}
	if payload.TargetVersion != "dev-abcdef1" {
		t.Fatalf("TargetVersion = %q, want dev-abcdef1", payload.TargetVersion)
	}
	if payload.TargetCommit != "abcdef123456" {
		t.Fatalf("TargetCommit = %q, want abcdef123456", payload.TargetCommit)
	}
	if len(payload.Logs) != 1 || payload.Logs[0].Message != "docker compose pull clirelay" {
		t.Fatalf("Logs = %+v, want compose pull log", payload.Logs)
	}

	close(releaseRunner)
}

func TestUpdaterStatusParsesSQLiteMigrationProgress(t *testing.T) {
	server := newUpdaterServer(updaterConfig{Token: "secret"})
	runID := server.startUpdate("cli-proxy-api", updateRequest{})

	reporter := updaterRunReporter{server: server, runID: runID}
	reporter.Stage("migrating", "checking legacy SQLite migration before service restart")
	reporter.Log("stderr", "clirelay sqlite migration: legacy SQLite found at /CLIProxyAPI/data/usage.db")
	reporter.Log("stderr", "clirelay sqlite migration: applying SQLite import into PostgreSQL")
	reporter.Log("stderr", "clirelay-migrate  | sqlite import progress: table 16/17 request_logs")
	reporter.Log("stderr", "clirelay-migrate  | sqlite import progress: table request_logs inserted_rows=2 target_rows=167648")

	payload := server.snapshot()
	if payload.Stage != "migrating" {
		t.Fatalf("Stage = %q, want migrating", payload.Stage)
	}
	if payload.ProgressPercent < 84 || payload.ProgressPercent >= 86 {
		t.Fatalf("ProgressPercent = %.2f, want row-based progress inside table 16/17", payload.ProgressPercent)
	}
	if payload.Migration == nil {
		t.Fatal("Migration = nil, want migration detail")
	}
	if payload.Migration.TargetDatabase != "PostgreSQL" {
		t.Fatalf("TargetDatabase = %q, want PostgreSQL", payload.Migration.TargetDatabase)
	}
	if payload.Migration.Phase != "applying" {
		t.Fatalf("Phase = %q, want applying", payload.Migration.Phase)
	}
	if payload.Migration.Table != "request_logs" || payload.Migration.TableIndex != 16 || payload.Migration.TableTotal != 17 {
		t.Fatalf("Migration table = %+v, want request_logs 16/17", payload.Migration)
	}
	if payload.Migration.InsertedRows != 2 || payload.Migration.TargetRows != 167648 {
		t.Fatalf("Migration rows = %+v, want inserted/target rows", payload.Migration)
	}

	reporter.Log("stderr", "clirelay-migrate  | sqlite import progress: table request_logs inserted_rows=167648 target_rows=167648")
	payload = server.snapshot()
	if payload.ProgressPercent <= 86 || payload.ProgressPercent >= 89 {
		t.Fatalf("ProgressPercent = %.2f, want row progress near completed table 16/17", payload.ProgressPercent)
	}
}

func TestUpdaterStatusMarksSQLiteMigrationSkipped(t *testing.T) {
	server := newUpdaterServer(updaterConfig{Token: "secret"})
	runID := server.startUpdate("cli-proxy-api", updateRequest{})

	reporter := updaterRunReporter{server: server, runID: runID}
	reporter.Stage("migrating", "checking legacy SQLite migration before service restart")
	reporter.Log("stderr", "clirelay sqlite migration: disabled by CLIRELAY_SQLITE_AUTO_MIGRATE")
	reporter.Stage("migrating", "legacy SQLite migration check finished before service restart")

	payload := server.snapshot()
	if payload.Message != "legacy SQLite migration skipped because auto-migration is disabled" {
		t.Fatalf("Message = %q, want disabled skip message", payload.Message)
	}
	if payload.Migration == nil {
		t.Fatal("Migration = nil, want skipped migration detail")
	}
	if payload.Migration.Phase != "skipped" || payload.Migration.SkipReason != "disabled" {
		t.Fatalf("Migration = %+v, want skipped disabled", payload.Migration)
	}
	if payload.ProgressPercent < 88 {
		t.Fatalf("ProgressPercent = %.2f, want skip progress near restart", payload.ProgressPercent)
	}
}

func TestUpdaterFailsWhenComposePullSkipsTargetService(t *testing.T) {
	called := make(chan struct{}, 1)
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:old\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	server := newUpdaterServer(updaterConfig{
		Token:   "secret",
		EnvFile: envFile,
		Runner: func(_ context.Context, _ string, _ string, _ string, service string, reporter updateReporter) error {
			reporter.Stage("pulling", "pulling image")
			reporter.Log("stdout", "docker compose pull "+service)
			reporter.Log("stderr", service+" Skipped")
			reporter.Stage("restarting", "restarting container")
			reporter.Log("stderr", "Container "+service+" Running")
			called <- struct{}{}
			return nil
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/update",
		strings.NewReader(`{"service":"cli-proxy-api","image":"ghcr.io/kittors/clirelay","tag":"dev","version":"dev-6704f60","commit":"6704f60ee834bce20e22fc65e67868801f483e32","channel":"dev"}`),
	)
	setUpdaterAuth(req)
	rec := httptest.NewRecorder()
	server.handleUpdate(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("update status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner")
	}

	eventually(t, time.Second, func() bool {
		return server.snapshot().Status == "failed"
	})

	payload := server.snapshot()
	if payload.Stage != "failed" {
		t.Fatalf("Stage = %q, want failed", payload.Stage)
	}
	if !strings.Contains(payload.Message, "pull skipped") {
		t.Fatalf("Message = %q, want pull skipped hint", payload.Message)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if string(data) != "CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:old\n" {
		t.Fatalf("env file content = %q, want previous image restored", string(data))
	}
}

func TestBuildComposeArgsIncludesProjectName(t *testing.T) {
	args := buildComposeArgs(
		"/workspace/docker-compose.yml",
		"/workspace/.env",
		"cliproxy",
		"up",
		"-d",
		"cli-proxy-api",
	)

	want := []string{
		"compose",
		"--project-name", "cliproxy",
		"--env-file", "/workspace/.env",
		"-f", "/workspace/docker-compose.yml",
		"up", "-d", "cli-proxy-api",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestRunComposeUpdateRecreatesOnlyTargetService(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	dockerPath := filepath.Join(dir, "docker")
	composePath := filepath.Join(dir, "docker-compose.yml")
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COMPOSE_LOG\"\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	if err := os.WriteFile(composePath, []byte("services:\n  clirelay:\n    image: clirelay\n  postgres:\n    image: postgres:15-alpine\n  redis:\n    image: redis:7-alpine\n"), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("CLI_PROXY_IMAGE=clirelay\n"), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOSE_LOG", logPath)

	err := runComposeUpdate(context.Background(), composePath, envPath, "cliproxy", "clirelay", updaterRunReporter{})
	if err != nil {
		t.Fatalf("runComposeUpdate failed: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read compose log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " pull clirelay",
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " up -d postgres redis",
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " up -d --no-deps --remove-orphans clirelay",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose commands = %#v, want %#v", got, want)
	}
}

func TestRunComposeUpdateUsesEnvFileNextToComposeWhenUnset(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	dockerPath := filepath.Join(dir, "docker")
	composePath := filepath.Join(dir, "docker-compose.yml")
	inferredEnvPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COMPOSE_LOG\"\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	if err := os.WriteFile(composePath, []byte("services:\n  clirelay:\n    image: clirelay\n  postgres:\n    image: postgres:15-alpine\n  redis:\n    image: redis:7-alpine\n"), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	if err := os.WriteFile(inferredEnvPath, []byte("CLI_PROXY_IMAGE=clirelay\n"), 0o644); err != nil {
		t.Fatalf("write inferred env: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOSE_LOG", logPath)

	err := runComposeUpdate(context.Background(), composePath, "", "cliproxy", "clirelay", updaterRunReporter{})
	if err != nil {
		t.Fatalf("runComposeUpdate failed: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read compose log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"compose --project-name cliproxy --env-file " + inferredEnvPath + " -f " + composePath + " pull clirelay",
		"compose --project-name cliproxy --env-file " + inferredEnvPath + " -f " + composePath + " up -d postgres redis",
		"compose --project-name cliproxy --env-file " + inferredEnvPath + " -f " + composePath + " up -d --no-deps --remove-orphans clirelay",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose commands = %#v, want %#v", got, want)
	}
}

func TestRunComposeUpdateUpgradesLegacySQLiteComposeWithRuntimeStack(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	dockerPath := filepath.Join(dir, "docker")
	composePath := filepath.Join(dir, "docker-compose.yml")
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COMPOSE_LOG\"\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	if err := os.WriteFile(composePath, []byte("services:\n  clirelay:\n    image: clirelay\n"), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("CLI_PROXY_IMAGE=clirelay\n"), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOSE_LOG", logPath)

	err := runComposeUpdate(context.Background(), composePath, envPath, "cliproxy", "clirelay", updaterRunReporter{})
	if err != nil {
		t.Fatalf("runComposeUpdate failed: %v", err)
	}

	composeData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read upgraded compose: %v", err)
	}
	composeText := string(composeData)
	for _, want := range []string{
		"clirelay-init:",
		"postgres:",
		"redis:",
		"clirelay-updater:",
		"/clirelay-deploy/.env",
		"service_completed_successfully",
	} {
		if !strings.Contains(composeText, want) {
			t.Fatalf("upgraded compose missing %q:\n%s", want, composeText)
		}
	}
	for _, forbidden := range []string{
		"clirelay-migrate:",
		"CLIRELAY_UPDATER_TOKEN is required",
		"POSTGRES_PASSWORD: ${CLIRELAY_POSTGRES_PASSWORD:-cliproxy}",
	} {
		if strings.Contains(composeText, forbidden) {
			t.Fatalf("upgraded compose still contains blocking fallback %q:\n%s", forbidden, composeText)
		}
	}

	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read upgraded env: %v", err)
	}
	envText := string(envData)
	for _, want := range []string{
		"CLIRELAY_POSTGRES_DSN=postgres://",
		"CLIRELAY_REDIS_ENABLE=true",
		"CLIRELAY_TARGET_SERVICE=clirelay",
	} {
		if !strings.Contains(envText, want) {
			t.Fatalf("upgraded env missing %q:\n%s", want, envText)
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read compose log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " pull clirelay",
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " up -d postgres redis",
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " up -d --no-deps --remove-orphans clirelay",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose commands = %#v, want %#v", got, want)
	}
}

func TestRunComposeUpdateBootstrapsProductionLegacySQLiteStack(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	dockerPath := filepath.Join(dir, "docker")
	composePath := filepath.Join(dir, "docker-compose.yml")
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COMPOSE_LOG\"\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	composeText := `services:
  cli-proxy-api:
    container_name: cli-proxy-api
    image: ${CLI_PROXY_IMAGE:-ghcr.io/kittors/clirelay:latest}
    restart: unless-stopped
    ports:
      - "8317:8317"
    volumes:
      - /root/cliproxy/data:/CLIProxyAPI/data
      - /root/cliproxy/logs:/CLIProxyAPI/logs
    environment:
      GIN_MODE: release
  clirelay-updater:
    container_name: clirelay-updater
    image: ${CLI_PROXY_IMAGE:-ghcr.io/kittors/clirelay:latest}
    command:
      - ./clirelay-updater
    environment:
      CLIRELAY_COMPOSE_FILE: /workspace/docker-compose.yml
      CLIRELAY_ENV_FILE: /workspace/.env
      CLIRELAY_TARGET_SERVICE: cli-proxy-api
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /root/cliproxy/docker-compose.yml:/workspace/docker-compose.yml:ro
      - /root/cliproxy/.env:/workspace/.env
    restart: unless-stopped
`
	if err := os.WriteFile(composePath, []byte(composeText), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:dev\n"), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOSE_LOG", logPath)

	err := runComposeUpdate(context.Background(), composePath, envPath, "cliproxy", "cli-proxy-api", updaterRunReporter{})
	if err != nil {
		t.Fatalf("runComposeUpdate failed: %v", err)
	}

	composeData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read upgraded compose: %v", err)
	}
	upgraded := string(composeData)
	for _, want := range []string{"cli-proxy-api:", "clirelay-updater:", "clirelay-init:", "postgres:", "redis:"} {
		if !strings.Contains(upgraded, want) {
			t.Fatalf("upgraded compose missing %q:\n%s", want, upgraded)
		}
	}
	if strings.Contains(upgraded, "clirelay-migrate:") {
		t.Fatalf("upgraded compose still contains SQLite migration service:\n%s", upgraded)
	}
	if strings.Contains(upgraded, "${CLI_PROXY_IMAGE:-${CLI_PROXY_IMAGE:-") {
		t.Fatalf("upgraded compose contains nested CLI_PROXY_IMAGE fallback:\n%s", upgraded)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(composeData, &doc); err != nil {
		t.Fatalf("parse upgraded compose: %v", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		t.Fatalf("services not found in upgraded compose:\n%s", upgraded)
	}
	target, ok := stringMap(services["cli-proxy-api"])
	if !ok {
		t.Fatalf("target service missing:\n%s", upgraded)
	}
	if target["container_name"] != "cli-proxy-api" {
		t.Fatalf("container_name = %#v, want preserved", target["container_name"])
	}
	if !reflect.DeepEqual(target["ports"], []any{"8317:8317"}) {
		t.Fatalf("ports = %#v, want preserved", target["ports"])
	}
	targetEnv, ok := stringMap(target["environment"])
	if !ok {
		t.Fatalf("target environment is not a map:\n%s", upgraded)
	}
	if targetEnv["GIN_MODE"] != "release" {
		t.Fatalf("GIN_MODE = %#v, want release", targetEnv["GIN_MODE"])
	}
	if targetEnv["CLIRELAY_SQLITE_AUTO_MIGRATE"] != "false" {
		t.Fatalf("target CLIRELAY_SQLITE_AUTO_MIGRATE = %#v, want false", targetEnv["CLIRELAY_SQLITE_AUTO_MIGRATE"])
	}
	updater, ok := stringMap(services["clirelay-updater"])
	if !ok {
		t.Fatalf("updater service missing:\n%s", upgraded)
	}
	updaterEnv, ok := stringMap(updater["environment"])
	if !ok {
		t.Fatalf("updater environment is not a map:\n%s", upgraded)
	}
	if updaterEnv["CLIRELAY_COMPOSE_FILE"] != "${CLIRELAY_PROJECT_DIR:-"+dir+"}/docker-compose.yml" {
		t.Fatalf("updater compose file = %#v, want writable project compose path", updaterEnv["CLIRELAY_COMPOSE_FILE"])
	}
	updaterVolumes, ok := updater["volumes"].([]any)
	if !ok || !containsAnyString(updaterVolumes, "${CLIRELAY_PROJECT_DIR:-"+dir+"}:${CLIRELAY_PROJECT_DIR:-"+dir+"}") {
		t.Fatalf("updater volumes = %#v, want writable project dir mount", updater["volumes"])
	}

	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read upgraded env: %v", err)
	}
	envText := string(envData)
	for _, want := range []string{
		"CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:dev\n",
		"CLIRELAY_POSTGRES_DSN=postgres://",
		"CLIRELAY_REDIS_ENABLE=true\n",
		"CLIRELAY_TARGET_SERVICE=cli-proxy-api\n",
	} {
		if !strings.Contains(envText, want) {
			t.Fatalf("upgraded env missing %q:\n%s", want, envText)
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read compose log: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " pull cli-proxy-api",
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " up -d postgres redis",
		"compose --project-name cliproxy --env-file " + envPath + " -f " + composePath + " up -d --no-deps --remove-orphans cli-proxy-api",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compose commands = %#v, want %#v", got, want)
	}
}

func TestUpgradeComposeRuntimeStackPreservesListEnvironment(t *testing.T) {
	upgraded, _, err := upgradeComposeRuntimeStack(`
services:
  clirelay:
    image: ghcr.io/kittors/clirelay:dev
    environment:
      - AUTH_PATH=/root/.cli-proxy-api
      - LEGACY_FLAG=1
`, "/opt/clirelay", "clirelay")
	if err != nil {
		t.Fatalf("upgradeComposeRuntimeStack failed: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(upgraded), &doc); err != nil {
		t.Fatalf("parse upgraded compose: %v", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		t.Fatalf("services not found in upgraded compose:\n%s", upgraded)
	}
	clirelay, ok := stringMap(services["clirelay"])
	if !ok {
		t.Fatalf("clirelay service not found in upgraded compose:\n%s", upgraded)
	}
	env, ok := stringMap(clirelay["environment"])
	if !ok {
		t.Fatalf("clirelay environment is not a map:\n%s", upgraded)
	}
	for key, want := range map[string]any{
		"AUTH_PATH":   "/root/.cli-proxy-api",
		"LEGACY_FLAG": "1",
	} {
		if env[key] != want {
			t.Fatalf("environment[%s] = %v, want %v", key, env[key], want)
		}
	}
	for _, forbidden := range runtimeStackEnvKeys() {
		if _, ok := env[forbidden]; ok {
			t.Fatalf("environment still contains generated runtime key %s: %#v", forbidden, env)
		}
	}
	if env["CLIRELAY_SQLITE_AUTO_MIGRATE"] != "false" {
		t.Fatalf("clirelay must skip startup SQLite migration, environment = %#v", env)
	}
	if got := clirelay["entrypoint"]; got == nil {
		t.Fatalf("clirelay service missing source-env entrypoint:\n%s", upgraded)
	}
	if !reflect.DeepEqual(clirelay["command"], []any{"./CLIProxyAPI"}) {
		t.Fatalf("clirelay command = %#v, want ./CLIProxyAPI\n%s", clirelay["command"], upgraded)
	}
	volumes, ok := clirelay["volumes"].([]any)
	if !ok || !containsAnyString(volumes, "${CLIRELAY_PROJECT_DIR:-/opt/clirelay}:/clirelay-deploy") {
		t.Fatalf("clirelay service missing /clirelay-deploy volume: %#v\n%s", clirelay["volumes"], upgraded)
	}
	for _, name := range []string{"clirelay-init", "postgres", "redis", "clirelay-updater"} {
		if _, ok := services[name]; !ok {
			t.Fatalf("upgraded compose missing service %s:\n%s", name, upgraded)
		}
	}
	if _, ok := services["clirelay-migrate"]; ok {
		t.Fatalf("upgraded compose still contains SQLite migration service:\n%s", upgraded)
	}
	for _, name := range []string{"clirelay-init", "clirelay-updater"} {
		service, ok := stringMap(services[name])
		if !ok {
			t.Fatalf("%s service not found:\n%s", name, upgraded)
		}
		healthcheck, ok := stringMap(service["healthcheck"])
		if !ok || healthcheck["disable"] != true {
			t.Fatalf("%s healthcheck = %#v, want disabled\n%s", name, service["healthcheck"], upgraded)
		}
	}
}

func TestImageFallbackUnwrapsCliProxyImageDefault(t *testing.T) {
	got := imageFallback("${CLI_PROXY_IMAGE:-ghcr.io/kittors/clirelay:latest}")
	if got != "ghcr.io/kittors/clirelay:latest" {
		t.Fatalf("imageFallback = %q, want literal image", got)
	}
}

func TestUpgradeComposeRuntimeStackKeepsGeneratedServicesOnTargetNetwork(t *testing.T) {
	upgraded, _, err := upgradeComposeRuntimeStack(`
services:
  cli-proxy-api:
    image: ghcr.io/kittors/clirelay:dev
    networks:
      - clirelay
networks:
  clirelay:
    name: clirelay
`, "/root/cliproxy", "cli-proxy-api")
	if err != nil {
		t.Fatalf("upgradeComposeRuntimeStack failed: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(upgraded), &doc); err != nil {
		t.Fatalf("parse upgraded compose: %v", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		t.Fatalf("services not found in upgraded compose:\n%s", upgraded)
	}
	target, ok := stringMap(services["cli-proxy-api"])
	if !ok {
		t.Fatalf("target service missing:\n%s", upgraded)
	}
	wantNetworks := target["networks"]
	for _, name := range []string{"postgres", "redis", "clirelay-updater"} {
		service, ok := stringMap(services[name])
		if !ok {
			t.Fatalf("service %s missing:\n%s", name, upgraded)
		}
		if !reflect.DeepEqual(service["networks"], wantNetworks) {
			t.Fatalf("%s networks = %#v, want %#v\n%s", name, service["networks"], wantNetworks, upgraded)
		}
	}
}

func TestEnsureRuntimeDataStackConfigUpgradesStackWithoutInitService(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	envPath := filepath.Join(dir, ".env")
	composeText := `services:
  clirelay:
    image: ghcr.io/kittors/clirelay:dev
    environment:
      CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN:?CLIRELAY_UPDATER_TOKEN is required for updater sidecar}
  postgres:
    image: postgres:15-alpine
  redis:
    image: redis:7-alpine
`
	if err := os.WriteFile(composePath, []byte(composeText), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	prepared, err := ensureRuntimeDataStackConfig(context.Background(), composePath, envPath, "clirelay", updaterRunReporter{})
	if err != nil {
		t.Fatalf("ensureRuntimeDataStackConfig failed: %v", err)
	}
	if prepared != envPath {
		t.Fatalf("prepared env path = %q, want %q", prepared, envPath)
	}
	upgradedData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	upgraded := string(upgradedData)
	for _, want := range []string{"clirelay-init:", "/clirelay-deploy/.env", "service_completed_successfully"} {
		if !strings.Contains(upgraded, want) {
			t.Fatalf("compose missing %q:\n%s", want, upgraded)
		}
	}
	if strings.Contains(upgraded, "clirelay-migrate:") {
		t.Fatalf("compose still contains SQLite migration service:\n%s", upgraded)
	}
	if strings.Contains(upgraded, "CLIRELAY_UPDATER_TOKEN is required") {
		t.Fatalf("compose still requires updater token:\n%s", upgraded)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	for _, want := range []string{"CLIRELAY_UPDATER_TOKEN=", "CLIRELAY_POSTGRES_DSN=postgres://", "CLIRELAY_REDIS_ENABLE=true"} {
		if !strings.Contains(string(envData), want) {
			t.Fatalf("env missing %q:\n%s", want, envData)
		}
	}
}

func TestEnsureRuntimeDataStackConfigRemovesLegacyMigrationService(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	envPath := filepath.Join(dir, ".env")
	composeText := `services:
  clirelay:
    image: ghcr.io/kittors/clirelay:dev
    depends_on:
      clirelay-migrate:
        condition: service_completed_successfully
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
  clirelay-init:
    image: ghcr.io/kittors/clirelay:dev
  clirelay-migrate:
    image: ghcr.io/kittors/clirelay:dev
    command: ["migrate-sqlite-to-postgres.sh"]
  postgres:
    image: postgres:15-alpine
  redis:
    image: redis:7-alpine
`
	if err := os.WriteFile(composePath, []byte(composeText), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	if _, err := ensureRuntimeDataStackConfig(context.Background(), composePath, envPath, "clirelay", updaterRunReporter{}); err != nil {
		t.Fatalf("ensureRuntimeDataStackConfig failed: %v", err)
	}
	upgradedData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	upgraded := string(upgradedData)
	if strings.Contains(upgraded, "clirelay-migrate") || strings.Contains(upgraded, "migrate-sqlite-to-postgres.sh") {
		t.Fatalf("compose still contains legacy SQLite migration wiring:\n%s", upgraded)
	}
}

func TestEnsureRuntimeEnvFileReplacesWorkspaceProjectDir(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(strings.Join([]string{
		"CLIRELAY_PROJECT_DIR=/workspace",
		"CLIRELAY_POSTGRES_DATA_PATH=/workspace/postgres-data",
		"CLIRELAY_REDIS_DATA_PATH=/workspace/redis-data",
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if err := ensureRuntimeEnvFile(context.Background(), envPath, "/root/cliproxy", "cli-proxy-api", "ghcr.io/kittors/clirelay:dev", updaterRunReporter{}); err != nil {
		t.Fatalf("ensureRuntimeEnvFile failed: %v", err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"CLIRELAY_PROJECT_DIR=/root/cliproxy\n",
		"CLIRELAY_POSTGRES_DATA_PATH=/root/cliproxy/postgres-data\n",
		"CLIRELAY_REDIS_DATA_PATH=/root/cliproxy/redis-data\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("env missing %q:\n%s", want, content)
		}
	}
}

func TestProjectDirFromMountsMapsWorkspaceFileMountToHostDir(t *testing.T) {
	projectDir, ok := projectDirFromMounts("/workspace/docker-compose.yml", []dockerMount{
		{Source: "/root/cliproxy/docker-compose.yml", Destination: "/workspace/docker-compose.yml", RW: false},
	})
	if !ok {
		t.Fatal("projectDirFromMounts did not resolve file mount")
	}
	if projectDir != "/root/cliproxy" {
		t.Fatalf("projectDir = %q, want /root/cliproxy", projectDir)
	}
}

func containsAnyString(items []any, want string) bool {
	for _, item := range items {
		if stringValue(item) == want {
			return true
		}
	}
	return false
}

func TestHostPathForMountedPathFindsExactReadOnlyFileMount(t *testing.T) {
	source, rel, dirMount, ok := hostPathForMountedPath("/opt/clirelay/docker-compose.yml", []dockerMount{
		{Source: "/srv/clirelay/docker-compose.yml", Destination: "/opt/clirelay/docker-compose.yml", RW: false},
	})
	if !ok {
		t.Fatal("hostPathForMountedPath did not find exact file mount")
	}
	if source != "/srv/clirelay/docker-compose.yml" || rel != "" || dirMount {
		t.Fatalf("source=%q rel=%q dirMount=%v", source, rel, dirMount)
	}
}

func TestHostPathForMountedPathFindsParentDirectoryMount(t *testing.T) {
	source, rel, dirMount, ok := hostPathForMountedPath("/opt/clirelay/docker-compose.yml", []dockerMount{
		{Source: "/srv", Destination: "/opt", RW: true},
		{Source: "/srv/clirelay", Destination: "/opt/clirelay", RW: true},
	})
	if !ok {
		t.Fatal("hostPathForMountedPath did not find directory mount")
	}
	if source != "/srv/clirelay" || rel != "docker-compose.yml" || !dirMount {
		t.Fatalf("source=%q rel=%q dirMount=%v", source, rel, dirMount)
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if condition() {
		return
	}
	t.Fatal("condition was not met before timeout")
}

func waitForUpdaterHealth(t *testing.T, addr string) {
	t.Helper()
	client := http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/health", nil)
		if err != nil {
			t.Fatalf("create health request: %v", err)
		}
		setUpdaterAuth(req)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("updater health did not become ready")
}
