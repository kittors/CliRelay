package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestMiniMaxOllamaDiscovery(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			previousURL, previousClient := ollamaCloudModelsURL, ollamaCloudModelsClient
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"data":[{"id":"minimax-m3"},{"id":"minimax-m2.7"},{"id":"minimax-m3.1-flash"},{"id":"gpt-oss:120b"}]}`))
			}))
			t.Cleanup(func() {
				server.Close()
				ollamaCloudModelsURL, ollamaCloudModelsClient = previousURL, previousClient
			})
			ollamaCloudModelsURL, ollamaCloudModelsClient = server.URL, server.Client()
			h := NewHandler(&config.Config{}, "", nil)
			rec := performModelDefinitionsRequest(h.GetStaticModelDefinitions, "ollama-cloud")
			var payload struct {
				Models []*registry.ModelInfo `json:"models"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, model := range payload.Models {
				ids = append(ids, model.ID)
				if model.ContextLength != 0 || model.MaxCompletionTokens != 0 {
					t.Fatalf("invented Ollama limits: %+v", model)
				}
			}
			if rec.Code != http.StatusOK || !reflect.DeepEqual(ids, []string{"minimax-m3.1-flash", "minimax-m3", "gpt-oss:120b"}) {
				t.Fatalf("discovery status=%d choices=%v", rec.Code, ids)
			}
		})
	}
}

func TestMiniMaxCommandCodeDiscovery(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			previousURL, previousClient := commandCodeModelsURL, commandCodeModelsClient
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"data":[
					{"id":"unrelated-model","name":"Untouched","context_length":12345},
					{"id":"MiniMaxAI/MiniMax-M3","name":"MiniMax M3","context_length":1000000},
					{"id":"minimax/minimax-m3.1-flash","name":"MiniMax M3.1 Flash","context_length":1000000},
					{"id":"minimax/minimax-m3.1-flash:free","name":"MiniMax M3.1 Flash","context_length":1000000},
					{"id":"MiniMaxAI/MiniMax-M2.7","context_length":200000},
					{"id":"MiniMaxAI/MiniMax-M2.5","context_length":200000}
				]}`))
			}))
			t.Cleanup(func() {
				server.Close()
				commandCodeModelsURL, commandCodeModelsClient = previousURL, previousClient
			})
			commandCodeModelsURL, commandCodeModelsClient = server.URL, server.Client()
			h := NewHandler(&config.Config{}, "", nil)
			rec := performModelDefinitionsRequest(h.GetStaticModelDefinitions, "commandcode")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			var payload struct {
				Models []*registry.ModelInfo `json:"models"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, model := range payload.Models {
				if strings.Contains(strings.ToLower(model.ID), "minimax") {
					ids = append(ids, model.ID)
					if model.ContextLength != 1000000 {
						t.Fatalf("gateway context changed: %+v", model)
					}
				}
			}
			want := []string{"minimax/minimax-m3.1-flash", "minimax/minimax-m3.1-flash:free", "MiniMaxAI/MiniMax-M3"}
			if !reflect.DeepEqual(ids, want) {
				t.Fatalf("MiniMax choices = %v, want %v", ids, want)
			}
			if status == http.StatusOK && (len(payload.Models) != 4 || payload.Models[0].ID != "unrelated-model" || payload.Models[0].ContextLength != 12345) {
				t.Fatalf("unrelated gateway metadata changed: %+v", payload.Models)
			}
		})
	}
}
