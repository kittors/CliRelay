package iflow

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func gzipBody(t *testing.T, raw string) io.ReadCloser {
	t.Helper()

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(raw)); err != nil {
		t.Fatalf("gzip write failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes()))
}

func TestFetchAPIKeyInfoRejectsOversizedCompressedResponse(t *testing.T) {
	limit := util.ProviderHTTPResponseLimit(iflowOAuthBodyLabel)
	oversized := strings.Repeat("a", int(limit)+1)

	auth := &IFlowAuth{
		httpClient: &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Encoding": []string{"gzip"},
					},
					Body: gzipBody(t, oversized),
				}, nil
			}),
		},
	}

	_, err := auth.fetchAPIKeyInfo(context.Background(), "sid=test")
	if err == nil || !strings.Contains(err.Error(), "response body exceeds") {
		t.Fatalf("fetchAPIKeyInfo error = %v, want response body exceeds", err)
	}
}

func TestRefreshAPIKeyRejectsOversizedCompressedResponse(t *testing.T) {
	limit := util.ProviderHTTPResponseLimit(iflowOAuthBodyLabel)
	oversized := strings.Repeat("b", int(limit)+1)

	auth := &IFlowAuth{
		httpClient: &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Encoding": []string{"gzip"},
					},
					Body: gzipBody(t, oversized),
				}, nil
			}),
		},
	}

	_, err := auth.RefreshAPIKey(context.Background(), "sid=test", "demo")
	if err == nil || !strings.Contains(err.Error(), "response body exceeds") {
		t.Fatalf("RefreshAPIKey error = %v, want response body exceeds", err)
	}
}

func TestSaveTokenToFileUsesPrivatePermissions(t *testing.T) {
	authFilePath := filepath.Join(t.TempDir(), "auth", "iflow-user.json")
	storage := &IFlowTokenStorage{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		APIKey:       "api-key",
		Email:        "user@example.com",
	}

	if err := storage.SaveTokenToFile(authFilePath); err != nil {
		t.Fatalf("SaveTokenToFile returned error: %v", err)
	}

	dirInfo, err := os.Stat(filepath.Dir(authFilePath))
	if err != nil {
		t.Fatalf("stat auth dir failed: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("auth dir permissions = %o, want 700", got)
	}

	fileInfo, err := os.Stat(authFilePath)
	if err != nil {
		t.Fatalf("stat auth file failed: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("auth file permissions = %o, want 600", got)
	}
}
