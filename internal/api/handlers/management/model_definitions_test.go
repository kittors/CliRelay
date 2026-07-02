package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func performModelDefinitionsRequest(handler gin.HandlerFunc, channel string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "channel", Value: channel}}
	c.Request = httptest.NewRequest(http.MethodGet, "/model-definitions/"+channel, nil)
	handler(c)
	return rec
}

func withClineRecommendedModelsServer(t *testing.T, status int, body string) {
	t.Helper()
	previousURL := clineRecommendedModelsURL
	previousClient := clineRecommendedModelsClient
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(func() {
		server.Close()
		clineRecommendedModelsURL = previousURL
		clineRecommendedModelsClient = previousClient
	})
	clineRecommendedModelsURL = server.URL + "/models"
	clineRecommendedModelsClient = server.Client()
}

func TestGetStaticModelDefinitionsUsesClineRecommendedModels(t *testing.T) {
	withClineRecommendedModelsServer(t, http.StatusOK, `{
		"clinePass": [
			{"id": "cline-pass/fresh", "name": "Fresh Model", "description": "from cline"},
			{"id": " ", "name": "ignored"}
		]
	}`)
	h := NewHandler(&config.Config{}, "", nil)

	rec := performModelDefinitionsRequest(h.GetStaticModelDefinitions, "cline")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Models []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
			OwnedBy     string `json:"owned_by"`
			Type        string `json:"type"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Models) != 1 {
		t.Fatalf("expected one remote model, got %+v", payload.Models)
	}
	model := payload.Models[0]
	if model.ID != "cline-pass/fresh" || model.DisplayName != "Fresh Model" || model.Description != "from cline" || model.OwnedBy != "cline" || model.Type != "cline" {
		t.Fatalf("unexpected model payload: %+v", model)
	}
}

func TestGetStaticModelDefinitionsFallsBackToClineStaticModels(t *testing.T) {
	withClineRecommendedModelsServer(t, http.StatusBadGateway, `{"error":"upstream unavailable"}`)
	h := NewHandler(&config.Config{}, "", nil)

	rec := performModelDefinitionsRequest(h.GetStaticModelDefinitions, "cline")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	for _, model := range payload.Models {
		if model.ID == "cline-pass/glm-5.2" {
			return
		}
	}
	t.Fatalf("expected static fallback to include cline-pass/glm-5.2, got %+v", payload.Models)
}
