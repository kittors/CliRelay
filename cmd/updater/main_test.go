package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

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

func TestUpdaterDefaultPathsDoNotPointAtWorkspace(t *testing.T) {
	server := newUpdaterServer(updaterConfig{})

	if strings.Contains(server.composeFile, "/workspace") {
		t.Fatalf("composeFile = %q, want no /workspace default", server.composeFile)
	}
	if strings.Contains(server.envFile, "/workspace") {
		t.Fatalf("envFile = %q, want no /workspace default", server.envFile)
	}
}

func TestRequestedImageRefDoesNotDuplicateExistingTag(t *testing.T) {
	if got := requestedImageRef("ghcr.io/zuohuadong/clirelay:dev", "dev"); got != "ghcr.io/zuohuadong/clirelay:dev" {
		t.Fatalf("requestedImageRef(tagged dev) = %q", got)
	}
	if got := requestedImageRef("localhost:5000/clirelay:latest", "latest"); got != "localhost:5000/clirelay:latest" {
		t.Fatalf("requestedImageRef(registry port) = %q", got)
	}
	if got := requestedImageRef("ghcr.io/zuohuadong/clirelay", "dev"); got != "ghcr.io/zuohuadong/clirelay:dev" {
		t.Fatalf("requestedImageRef(untagged dev) = %q", got)
	}
}

func TestUpdaterPersistsRequestedImageBeforeComposeUpdate(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:dev\nOTHER=value\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	called := make(chan struct{}, 1)
	server := newUpdaterServer(updaterConfig{
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

func TestUpdaterRejectsRequestWhenEnvFileCannotBeUpdated(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(envDir, 0o500); err != nil {
		t.Fatalf("make readonly dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(envDir, 0o700)
	})

	server := newUpdaterServer(updaterConfig{
		EnvFile: filepath.Join(envDir, ".env"),
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
	rec := httptest.NewRecorder()

	server.handleUpdate(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to update env file") {
		t.Fatalf("body = %q, want env update failure", rec.Body.String())
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
	server := newUpdaterServer(updaterConfig{
		EnvFile: filepath.Join(t.TempDir(), ".env"),
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
	server.handleStatus(statusRec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
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

func TestUpdaterFailsWhenComposePullSkipsTargetService(t *testing.T) {
	called := make(chan struct{}, 1)
	server := newUpdaterServer(updaterConfig{
		EnvFile: filepath.Join(t.TempDir(), ".env"),
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
