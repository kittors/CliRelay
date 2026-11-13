package usage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type lifecycleCallbackPlugin func(context.Context, coreusage.Record)

func (f lifecycleCallbackPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	f(ctx, record)
}

// Close must not hold the DB lifecycle mutex while waiting for SDK callbacks:
// a previously admitted plugin may still finish by checking/initializing the
// store. The same default dispatcher must remain usable after reopening it.
func TestCloseDBAllowsAdmittedCallbackToFinishInit(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	oldPath := GetDBPath()
	oldModel := "callback-old-" + coreusage.NewIdempotencyKey()
	entered, release, initDone, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	coreusage.RegisterPlugin(lifecycleCallbackPlugin(func(_ context.Context, r coreusage.Record) {
		if r.Model != oldModel {
			return
		}
		close(entered)
		<-release
		if err := InitDB(oldPath, config.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
			t.Errorf("callback InitDB: %v", err)
		}
		close(initDone)
	}))
	contentPath := filepath.Join(t.TempDir(), "deferred-output")
	if err := os.WriteFile(contentPath, []byte("old response body"), 0600); err != nil {
		t.Fatal(err)
	}
	coreusage.PublishRecord(context.Background(), coreusage.Record{Model: oldModel, RequestedAt: time.Date(2026, 11, 11, 23, 30, 0, 0, time.UTC), InputContent: "old request body", OutputContentPath: contentPath, Detail: coreusage.Detail{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}})
	awaitUsageLifecycle(t, entered, "admitted callback")
	go func() { CloseDB(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("CloseDB passed an unfinished callback")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	awaitUsageLifecycle(t, initDone, "callback InitDB without lifecycle-lock inversion")
	awaitUsageLifecycle(t, closed, "CloseDB after callback")
	if _, err := os.Stat(contentPath); !os.IsNotExist(err) {
		t.Errorf("temporary body not cleaned before close returned: %v", err)
	}
	assertLifecycleStoredRecord(t, oldPath, oldModel, "old request body", "old response body", 5)
	newPath := filepath.Join(t.TempDir(), "reopened.db")
	if err := InitDB(newPath, config.RequestLogStorageConfig{StoreContent: true}, time.FixedZone("next-store", 9*3600)); err != nil {
		t.Fatal(err)
	}
	coreusage.PublishRecord(context.Background(), coreusage.Record{Model: "callback-new", RequestedAt: time.Now(), InputContent: "new request body", OutputContent: "new response body", Detail: coreusage.Detail{TotalTokens: 7}})
	CloseDB()
	assertLifecycleStoredRecord(t, newPath, "callback-new", "new request body", "new response body", 7)
	// Reopening and persisting in a new store must not duplicate the old record.
	assertLifecycleStoredRecord(t, oldPath, oldModel, "old request body", "old response body", 5)
}

func assertLifecycleStoredRecord(t *testing.T, path, wantModel, wantInput, wantOutput string, wantTokens int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("store %s has %d rows, want exactly one", path, count)
	}
	var model, compression string
	var tokens int64
	var input, output []byte
	if err := db.QueryRow("SELECT l.model,l.total_tokens,c.compression,c.input_content,c.output_content FROM request_logs l JOIN request_log_content c ON c.log_id=l.id").Scan(&model, &tokens, &compression, &input, &output); err != nil {
		t.Fatal(err)
	}
	inputText, inputErr := decompressLogContent(compression, input)
	outputText, outputErr := decompressLogContent(compression, output)
	if model != wantModel || tokens != wantTokens || inputText != wantInput || outputText != wantOutput || inputErr != nil || outputErr != nil {
		t.Errorf("record model=%q tokens=%d input=%q/%v output=%q/%v", model, tokens, inputText, inputErr, outputText, outputErr)
	}
	var requests, rollupTokens int64
	if err := db.QueryRow("SELECT SUM(request_count),SUM(total_tokens) FROM usage_rollup_buckets WHERE bucket_kind=?", rollupBucketLifetime).Scan(&requests, &rollupTokens); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || rollupTokens != wantTokens {
		t.Errorf("non-atomic/duplicate rollup: requests=%d tokens=%d", requests, rollupTokens)
	}
}

// User hooks are outside the store lease. Otherwise Close can pause SDK
// admission and wait for a direct writer whose hook is trying to Publish.
func TestCloseDBReleasesWriteLeaseBeforePublicationHooks(t *testing.T) {
	for _, kind := range []string{"settled", "tokens"} {
		t.Run(kind, func(t *testing.T) {
			initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
			oldPath := GetDBPath()
			entered, release, written, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			const requestID = "lifecycle-hook-request"
			const apiKey = "lifecycle-hook-key"
			hook := func() {
				close(entered)
				<-release
				coreusage.PublishRecord(context.Background(), coreusage.Record{Model: "hook-new", RequestedAt: time.Now(), InputContent: "new request", OutputContent: "new response", Detail: coreusage.Detail{TotalTokens: 2}})
			}
			if kind == "settled" {
				previous := requestLogSettled.Load()
				t.Cleanup(func() { requestLogSettled.Store(previous) })
				SetRequestLogSettledHook(func(id string, _ bool, _ float64, _ int64) {
					if id == requestID {
						hook()
					}
				})
			} else {
				previous := tokenUsageCallback
				t.Cleanup(func() { tokenUsageCallback = previous })
				SetTokenUsageCallback(func(key, _ string, _ int64) {
					if key == apiKey {
						hook()
					}
				})
			}
			go func() {
				InsertRequestLog(RequestLogEntry{RequestID: requestID, APIKey: apiKey, Model: "hook-old", Timestamp: time.Now(), InputContent: "old request", OutputContent: "old response", Tokens: TokenStats{TotalTokens: 3}})
				close(written)
			}()
			awaitUsageLifecycle(t, entered, "external "+kind+" callback")
			go func() { CloseDB(); close(closed) }()
			awaitUsageLifecycle(t, closed, "close must not wait for external "+kind+" callback")
			assertLifecycleStoredRecord(t, oldPath, "hook-old", "old request", "old response", 3)
			newPath := filepath.Join(t.TempDir(), "hook-new.db")
			if err := InitDB(newPath, config.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
				t.Fatal(err)
			}
			close(release)
			awaitUsageLifecycle(t, written, "recursive publication from "+kind+" callback")
			CloseDB()
			assertLifecycleStoredRecord(t, newPath, "hook-new", "new request", "new response", 2)
		})
	}
}
