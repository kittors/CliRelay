package management

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// End-to-end for the reported bug: selecting an image model in the catalog and
// pressing Test must reach the images endpoint and come back with a picture, not
// run a chat completion and report the prose as a pass.

// signedPNG builds a one-chunk PNG whose caBX manifest names a generator, so the
// probe's provenance reporting is exercised on a real container rather than on a
// mocked summary.
func signedPNG(generator, version string) []byte {
	manifest := []byte{0x00, 0x00, 0x00, 0x40}
	manifest = append(manifest, "jumb"...)
	manifest = append(manifest, make([]byte, 16)...)
	manifest = append(manifest, "c2pa"...)
	appendText := func(value string) {
		manifest = append(manifest, byte(0x60|len(value)))
		manifest = append(manifest, value...)
	}
	for _, token := range []string{"claim_generator_info", "name", generator, "version", version} {
		appendText(token)
	}

	out := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	writeChunk := func(chunkType string, data []byte) {
		length := make([]byte, 4)
		binary.BigEndian.PutUint32(length, uint32(len(data)))
		out = append(out, length...)
		out = append(out, chunkType...)
		out = append(out, data...)
		out = append(out, 0, 0, 0, 0)
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 1024)
	binary.BigEndian.PutUint32(ihdr[4:8], 768)
	writeChunk("IHDR", ihdr)
	writeChunk("caBX", manifest)
	writeChunk("IDAT", []byte{0x00})
	return out
}

// modelTestImageExecutor answers an images request with one signed PNG.
type modelTestImageExecutor struct {
	alt      string
	model    string
	payload  string
	metadata map[string]any
	calls    int
}

func (e *modelTestImageExecutor) Identifier() string { return "codex" }

func (e *modelTestImageExecutor) Execute(
	_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options,
) (coreexecutor.Response, error) {
	e.calls++
	e.alt = opts.Alt
	e.model = req.Model
	e.payload = string(req.Payload)
	e.metadata = opts.Metadata
	encoded := base64.StdEncoding.EncodeToString(signedPNG("gpt-image", "2.0"))
	body := `{"created":1,"data":[{"b64_json":"` + encoded + `","revised_prompt":"a red teapot"}]}`
	return coreexecutor.Response{Payload: []byte(body)}, nil
}

func (e *modelTestImageExecutor) ExecuteStream(
	context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	return nil, http.ErrNotSupported
}

func (e *modelTestImageExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *modelTestImageExecutor) CountTokens(
	context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, http.ErrNotSupported
}

func (e *modelTestImageExecutor) HttpRequest(
	context.Context, *coreauth.Auth, *http.Request,
) (*http.Response, error) {
	return nil, http.ErrNotSupported
}

type modelTestTaskBody struct {
	OK     bool   `json:"ok"`
	Mode   string `json:"mode"`
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Result struct {
		Kind   string `json:"kind"`
		Images []struct {
			B64JSON string `json:"b64_json"`
			Format  string `json:"format"`
			Width   int    `json:"width"`
			Height  int    `json:"height"`
			C2PA    struct {
				Present          bool   `json:"present"`
				Generator        string `json:"generator"`
				GeneratorVersion string `json:"generator_version"`
			} `json:"c2pa"`
		} `json:"images"`
	} `json:"result"`
	Metadata struct {
		Mode              string `json:"mode"`
		RequestedModel    string `json:"requested_model"`
		Upstream          string `json:"upstream_endpoint"`
		ProvenanceModel   string `json:"provenance_model"`
		ProvenanceVersion string `json:"provenance_version"`
		ProvenanceMatches *bool  `json:"provenance_matches"`
	} `json:"metadata"`
	Request map[string]any `json:"request"`
}

// labelManagementImageAuth gives a registered credential a channel name, which
// is what an operator picks in the panel's channel selector.
func labelManagementImageAuth(t *testing.T, manager *coreauth.Manager, id, label string) {
	t.Helper()
	auth, ok := manager.GetByID(id)
	if !ok || auth == nil {
		t.Fatalf("auth %q is not registered", id)
	}
	updated := auth.Clone()
	updated.Label = label
	if _, err := manager.Register(context.Background(), updated); err != nil {
		t.Fatalf("label auth: %v", err)
	}
}

func startModelTest(t *testing.T, h *Handler, body string) modelTestTaskBody {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/models/test", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PostModelTest(c)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	var decoded modelTestTaskBody
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	return decoded
}

func waitForModelTest(t *testing.T, h *Handler, taskID string) modelTestTaskBody {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set(managementPrincipalKey, identity.Principal{
			EffectiveTenant: identity.Tenant{ID: identity.SystemTenantID},
		})
		c.Params = gin.Params{{Key: "task_id", Value: taskID}}
		c.Request = httptest.NewRequest(http.MethodGet, "/models/test/"+taskID, nil)

		h.GetModelTestTask(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var decoded modelTestTaskBody
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode poll response: %v", err)
		}
		if decoded.Status == "succeeded" || decoded.Status == "failed" {
			return decoded
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for model test %s, last body=%s", taskID, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestModelTestImageModelProducesAnImageNotProse(t *testing.T) {
	executor := &modelTestImageExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-model-test", "gpt-image-2.5-flare")
	// The probe restricts the run to the selected channel, so the registered
	// credential has to answer to that name or routing correctly finds nothing.
	labelManagementImageAuth(t, manager, "codex-model-test", "jd7@privaterelay.appleid.com")

	h := &Handler{authManager: manager}
	started := startModelTest(t, h, `{
		"model":"gpt-image-2.5-flare",
		"prompt":"a red ceramic teapot",
		"channel":"jd7@privaterelay.appleid.com",
		"size":"1024x1024",
		"quality":"high"
	}`)
	if started.Mode != string(modeImage) {
		t.Fatalf("mode = %q, want %q", started.Mode, modeImage)
	}
	if started.TaskID == "" {
		t.Fatal("an image probe must run as a task; a clip or a large image outlives a gateway timeout")
	}

	done := waitForModelTest(t, h, started.TaskID)
	if done.Status != "succeeded" {
		t.Fatalf("status = %q, body result kind = %q", done.Status, done.Result.Kind)
	}

	// The request went to the images endpoint, not to chat completions.
	if executor.alt != imageGenerationAlt {
		t.Fatalf("alt = %q, want %q", executor.alt, imageGenerationAlt)
	}
	if strings.Contains(executor.payload, "messages") {
		t.Fatalf("the upstream request must not be a chat completion: %s", executor.payload)
	}
	if !strings.Contains(executor.payload, `"quality":"high"`) {
		t.Fatalf("image options must reach the upstream: %s", executor.payload)
	}
	// The channel the operator picked must scope the run, which is what the
	// selector always implied.
	if got := executor.metadata["allowed-channels"]; got != "jd7@privaterelay.appleid.com" {
		t.Fatalf("allowed-channels = %v, want the selected channel", got)
	}

	if done.Result.Kind != "image" {
		t.Fatalf("result kind = %q, want image", done.Result.Kind)
	}
	if len(done.Result.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(done.Result.Images))
	}
	image := done.Result.Images[0]
	if image.Format != "png" || image.Width != 1024 || image.Height != 768 {
		t.Fatalf("image described as %s %dx%d", image.Format, image.Width, image.Height)
	}

	// The provenance the file claims is reported, and the mismatch with the
	// requested model is flagged rather than reported as a clean pass.
	if !image.C2PA.Present || image.C2PA.Generator != "gpt-image" {
		t.Fatalf("c2pa = %+v, want the embedded manifest", image.C2PA)
	}
	if done.Metadata.ProvenanceVersion != "2.0" {
		t.Fatalf("provenance version = %q, want 2.0", done.Metadata.ProvenanceVersion)
	}
	if done.Metadata.ProvenanceMatches == nil || *done.Metadata.ProvenanceMatches {
		t.Fatal("a 2.5 request served by a 2.0 manifest must be reported as a mismatch")
	}
	if done.Metadata.Upstream != "/v1/images/generations" {
		t.Fatalf("upstream = %q", done.Metadata.Upstream)
	}
}

// The request snapshot is what an operator reads to see what was actually sent;
// it must describe uploads rather than echo megabytes of base64 back at them.
func TestModelTestRequestSnapshotOmitsInlineMedia(t *testing.T) {
	executor := &modelTestImageExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-model-test-edit", "gpt-image-2.5-flare")

	upload := "data:image/png;base64," + base64.StdEncoding.EncodeToString(signedPNG("x", "1"))
	h := &Handler{authManager: manager}
	started := startModelTest(t, h, `{
		"model":"gpt-image-2.5-flare",
		"mode":"image_edit",
		"prompt":"replace the background",
		"images":["`+upload+`"]
	}`)

	encoded, err := json.Marshal(started.Request)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	snapshot := string(encoded)
	if strings.Contains(snapshot, "iVBOR") || strings.Contains(snapshot, upload[40:80]) {
		t.Fatalf("the snapshot must not carry the uploaded image: %s", snapshot)
	}
	if !strings.Contains(snapshot, "inline image/png") {
		t.Fatalf("the snapshot must describe the upload: %s", snapshot)
	}
	if !strings.Contains(snapshot, "replace the background") {
		t.Fatalf("the snapshot must keep the prompt: %s", snapshot)
	}

	done := waitForModelTest(t, h, started.TaskID)
	if done.Status != "succeeded" {
		t.Fatalf("status = %q", done.Status)
	}
	if executor.alt != imageEditsAlt {
		t.Fatalf("alt = %q, want %q for an edit", executor.alt, imageEditsAlt)
	}
}

// Polling an id the task store does not have is "not found", not "service
// unavailable". The all-routes smoke test treats a 503 as a broken route, and an
// operator refreshing a finished task should not be told the feature is down.
func TestGetModelTestTaskReportsNotFoundWithoutAnAuthManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "task_id", Value: "task-does-not-exist"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/models/test/task-does-not-exist", nil)

	(&Handler{}).GetModelTestTask(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}
