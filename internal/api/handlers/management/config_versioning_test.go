package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type versionedNode struct {
	h      *Handler
	engine *gin.Engine
}

func newVersionedNode(t *testing.T) versionedNode {
	t.Helper()
	cfg := &config.Config{}
	usage.ApplyStoredRuntimeSettings(cfg)
	h := NewHandler(cfg, filepath.Join(t.TempDir(), "config.yaml"), nil)
	engine := gin.New()
	engine.Use(CaptureConfigVersion())
	engine.GET("/debug", h.GetDebug)
	engine.PUT("/debug", h.PutDebug)
	engine.PUT("/request-retry", h.PutRequestRetry)
	engine.GET("/api-key-entries", h.GetAPIKeyEntries)
	engine.PUT("/api-key-entries", h.PutAPIKeyEntries)
	return versionedNode{h: h, engine: engine}
}

func (n versionedNode) do(t *testing.T, method, path, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	n.engine.ServeHTTP(rec, req)
	return rec
}

func TestSameSettingFromTwoNodesConflictsInsteadOfOverwriting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "versions.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(usage.CloseDB)
	nodeA, nodeB := newVersionedNode(t), newVersionedNode(t)

	// Both panels read debug before anyone wrote it.
	read := nodeB.do(t, http.MethodGet, "/debug", "", "")
	if etag := read.Header().Get("ETag"); etag != `"0"` {
		t.Fatalf("initial ETag = %q, want \"0\"", etag)
	}
	if rec := nodeA.do(t, http.MethodPut, "/debug", `{"value":true}`, `"0"`); rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("node A write = %d %s (ETag %q)", rec.Code, rec.Body.String(), rec.Header().Get("ETag"))
	}
	// Node B's operator saves the value they saw at version 0: rejected.
	rec := nodeB.do(t, http.MethodPut, "/debug", `{"value":false,"version":0}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale write status = %d, body %s; want 409", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "config_version_conflict" || body.Error.Details["current_version"] != float64(1) {
		t.Fatalf("conflict body = %s", rec.Body.String())
	}
	if payload, _ := usage.GetRuntimeSettingPayload("debug"); string(payload) != "true" {
		t.Fatalf("debug = %s, the rejected write must not land", payload)
	}

	// After refreshing (version 1) the same save goes through.
	if rec := nodeB.do(t, http.MethodPut, "/debug", `{"value":false}`, `"1"`); rec.Code != http.StatusOK {
		t.Fatalf("refreshed write = %d %s", rec.Code, rec.Body.String())
	}
	// A client without versions writes another key; node A's stale copy of
	// debug is not written back with it.
	if rec := nodeA.do(t, http.MethodPut, "/request-retry", `{"value":4}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("request-retry write = %d %s", rec.Code, rec.Body.String())
	}
	if payload, _ := usage.GetRuntimeSettingPayload("debug"); string(payload) != "false" {
		t.Fatalf("debug = %s after node A saved another key, node B's change was reverted", payload)
	}
	if !nodeA.h.cfg.Debug {
		t.Fatal("control: node A's live config is stale without a dispatcher, which is what made the revert possible")
	}
}

func TestWholeListSaveIsCheckedAgainstTheVersionItWasReadAt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "keys.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(usage.CloseDB)
	nodeA, nodeB := newVersionedNode(t), newVersionedNode(t)

	etag := nodeA.do(t, http.MethodGet, "/api-key-entries", "", "").Header().Get("ETag")
	// Node B adds a key through a full-list save.
	if rec := nodeB.do(t, http.MethodPut, "/api-key-entries", `[{"key":"sk-node-b-XXXXXXXX","name":"b"}]`, etag); rec.Code != http.StatusOK {
		t.Fatalf("node B save = %d %s", rec.Code, rec.Body.String())
	}
	// Node A saves the list it read before that: it would delete node B's key.
	rec := nodeA.do(t, http.MethodPut, "/api-key-entries", `[{"key":"sk-node-a-XXXXXXXX","name":"a"}]`, etag)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale full-list save = %d %s; want 409", rec.Code, rec.Body.String())
	}
	if usage.GetAPIKey("sk-node-b-XXXXXXXX") == nil {
		t.Fatal("node B's key was lost")
	}
}
