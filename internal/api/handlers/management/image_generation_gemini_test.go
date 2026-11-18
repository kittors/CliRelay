package management

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const geminiImageTestModel = "gemini-3.1-flash-image"

func registerAntigravityImageAuth(t *testing.T, manager *coreauth.Manager, id, label string) {
	t.Helper()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       id,
		Provider: "antigravity",
		Label:    label,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "antigravity", "email": id + "@example.com", "access_token": "token"},
	}); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{
		{ID: geminiImageTestModel, DisplayName: "Gemini 3.1 Flash Image"},
		{ID: "gemini-3.1-pro-high"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
}

func TestImageGenerationRoutesGeminiImageModelsToAntigravity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageProviderExecutor{identifier: "antigravity"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerAntigravityImageAuth(t, manager, "antigravity-image-route", "Design A")

	h := &Handler{authManager: manager}
	if _, err := h.executeImageGenerationTest(context.Background(),
		[]byte(`{"model":"gemini-3.1-flash-image","prompt":"a cat","aspect_ratio":"16:9","image_size":"2K","n":2}`),
		imageGenerationAlt,
	); err != nil {
		t.Fatalf("executeImageGenerationTest() error = %v", err)
	}
	if executor.calls != 2 {
		t.Fatalf("antigravity executor calls = %d, want 2 (n is split into single calls)", executor.calls)
	}
	if executor.alt != imageGenerationAlt || executor.model != geminiImageTestModel {
		t.Fatalf("alt/model = %q/%q", executor.alt, executor.model)
	}
	for _, payload := range executor.payloads {
		if gjson.Get(payload, "aspect_ratio").String() != "16:9" || gjson.Get(payload, "image_size").String() != "2K" || gjson.Get(payload, "n").Int() != 1 {
			t.Fatalf("payload = %s", payload)
		}
	}
}

func TestImageGenerationTestPayloadCarriesGeminiShape(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/image-generation/test",
		strings.NewReader(`{"model":"gemini-3.1-flash-image","prompt":"a cat","aspect_ratio":" 9:16 ","image_size":"4K"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	payload, alt, err := parseImageGenerationTestPayload(c)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if alt != imageGenerationAlt || gjson.GetBytes(payload, "aspect_ratio").String() != "9:16" || gjson.GetBytes(payload, "image_size").String() != "4K" {
		t.Fatalf("alt=%q payload=%s", alt, payload)
	}

	// Without the new fields the request is exactly what it used to be.
	c.Request = httptest.NewRequest(http.MethodPost, "/image-generation/test",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"a cat","size":"1024x1024","quality":"high"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	payload, _, err = parseImageGenerationTestPayload(c)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gjson.GetBytes(payload, "aspect_ratio").Exists() || gjson.GetBytes(payload, "image_size").Exists() {
		t.Fatalf("payload = %s, want no Gemini fields", payload)
	}
}

func TestImageGenerationMultipartEditCarriesGeminiShape(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for field, value := range map[string]string{
		"model":        geminiImageTestModel,
		"prompt":       "make it night",
		"aspect_ratio": "4:3",
		"image_size":   "1K",
	} {
		if err := writer.WriteField(field, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("image", "ref.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	_ = writer.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/image-generation/test", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	payload, alt, err := parseImageGenerationTestPayload(c)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if alt != imageEditsAlt || gjson.GetBytes(payload, "aspect_ratio").String() != "4:3" || gjson.GetBytes(payload, "image_size").String() != "1K" {
		t.Fatalf("alt=%q payload=%s", alt, payload)
	}
}

func TestListImageGenerationChannelsOffersAntigravityGeminiModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	registerAntigravityImageAuth(t, manager, "antigravity-image-channels", "Design B")

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/image-generation/channels", nil)
	h.ListImageGenerationChannels(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var response struct {
		Channels  []string `json:"channels"`
		Providers []struct {
			Provider string   `json:"provider"`
			Channels []string `json:"channels"`
			Models   []string `json:"models"`
		} `json:"providers"`
		Models []registry.ImageGenerationModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Contains(response.Channels, "Design B") {
		t.Fatalf("channels = %v, want the Antigravity channel", response.Channels)
	}
	var provider *struct {
		Provider string   `json:"provider"`
		Channels []string `json:"channels"`
		Models   []string `json:"models"`
	}
	for index := range response.Providers {
		if response.Providers[index].Provider == "antigravity" {
			provider = &response.Providers[index]
		}
	}
	if provider == nil || !slices.Equal(provider.Models, []string{geminiImageTestModel}) {
		t.Fatalf("providers = %+v, want antigravity serving only the image model", response.Providers)
	}
	var model *registry.ImageGenerationModel
	for index := range response.Models {
		if response.Models[index].ID == geminiImageTestModel {
			model = &response.Models[index]
		}
	}
	if model == nil || len(model.AspectRatios) != 14 || !slices.Equal(model.ImageSizes, []string{"512", "1K", "2K", "4K"}) || !model.SupportsEdit {
		t.Fatalf("model = %+v", model)
	}
}

func TestModelTestOptionsForGeminiImageModel(t *testing.T) {
	// Before any Antigravity credential serves it, the model is a chat model and
	// is probed as one.
	before := buildModelTestOptions(geminiImageTestModel, "")
	if before.Modes[0].Mode != modeText {
		t.Fatalf("modes without an images-capable credential = %+v, want the chat probe", before.Modes)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	registerAntigravityImageAuth(t, manager, "antigravity-image-options", "Design C")

	options := buildModelTestOptions(geminiImageTestModel, "")
	modes := make([]modelTestMode, 0, len(options.Modes))
	for _, mode := range options.Modes {
		modes = append(modes, mode.Mode)
	}
	if !slices.Equal(modes, []modelTestMode{modeImage, modeImageEdit}) {
		t.Fatalf("modes = %v", modes)
	}
	if len(options.Sizes) != 0 || len(options.Qualities) != 0 {
		t.Fatalf("WIDTHxHEIGHT sizes and gpt-image qualities must not be offered: %+v", options)
	}
	if !slices.Equal(options.AspectRatios, registry.GeminiImageAspectRatios()) || !slices.Equal(options.ImageSizes, registry.GeminiImageSizes()) {
		t.Fatalf("shape options = %v / %v", options.AspectRatios, options.ImageSizes)
	}
	if options.MaxImages <= 0 {
		t.Fatalf("max images = %d, editing needs uploads", options.MaxImages)
	}
	if err := validateModelTestProviders(geminiImageTestModel, modeImage); err != nil {
		t.Fatalf("validateModelTestProviders: %v", err)
	}

	// gpt-image keeps its form.
	codex := buildModelTestOptions("gpt-image-2", "")
	if len(codex.Sizes) == 0 || len(codex.Qualities) == 0 || len(codex.AspectRatios) != 0 {
		t.Fatalf("gpt-image options changed: %+v", codex)
	}
}

func TestBuildImagePayloadCarriesGeminiShape(t *testing.T) {
	payload, err := buildImageTestPayload(modelTestRequest{
		Model:       geminiImageTestModel,
		Prompt:      "a red teapot",
		AspectRatio: "21:9",
		ImageSize:   "2K",
	}, modeImage)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(payload, "aspect_ratio").String() != "21:9" || gjson.GetBytes(payload, "image_size").String() != "2K" {
		t.Fatalf("payload = %s", payload)
	}
	if gjson.GetBytes(payload, "size").Exists() || gjson.GetBytes(payload, "quality").Exists() {
		t.Fatalf("unset size/quality must stay out of the payload: %s", payload)
	}
}
