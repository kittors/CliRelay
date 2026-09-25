package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// A console task accepted by node A is answered by node B's handler from the
// shared snapshot, and an unknown id still gets the plain 404.
func TestClusterConsoleTaskPollReachesAnotherNode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	SetSharedJobSnapshots(jobsnapshot.NewMemoryStore())
	t.Cleanup(func() { SetSharedJobSnapshots(nil) })

	nodeA := NewHandler(&config.Config{}, "", coreauth.NewManager(nil, nil, nil))
	nodeB := NewHandler(&config.Config{}, "", coreauth.NewManager(nil, nil, nil))
	t.Cleanup(nodeA.Close)
	t.Cleanup(nodeB.Close)
	router := gin.New()
	router.GET("/image-generation/test/:task_id", nodeB.GetImageGenerationTestTask)

	// An unsupported model fails inside the task without reaching upstream,
	// which is all this test needs from the run.
	task := nodeA.ensureImageGenerationService().Start(identity.SystemTenantID, []byte(`{"model":"not-an-image-model","prompt":"a fox"}`), "images/generations")

	deadline := time.Now().Add(3 * time.Second)
	for {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/image-generation/test/"+task.ID, nil))
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"failed"`) {
			if !strings.Contains(rec.Body.String(), "not a supported image generation model") {
				t.Fatalf("node B served the task without its error: %s", rec.Body.String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node B never saw node A's task: %d %s", rec.Code, rec.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/image-generation/test/task-test", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("unknown task id = %d %s", rec.Code, rec.Body.String())
	}
}
