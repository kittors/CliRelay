# Request Log Database Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a safe one-click cleanup action on the Request Logs page that deletes only SQLite request-log history and message bodies.

**Architecture:** Extend the existing management usage-log API with a `DELETE /usage/logs` endpoint backed by a usage-layer table cleanup helper, then surface that endpoint from the Request Logs page with an explicit danger confirmation modal and immediate refresh.

**Tech Stack:** Go, Gin, SQLite (`modernc.org/sqlite`), React, TypeScript, Vitest

---

### Task 1: Backend usage-layer cleanup primitive

**Files:**
- Modify: `internal/usage/usage_db.go`
- Test: `internal/usage/usage_db_test.go`

- [ ] **Step 1: Write the failing usage-layer test**

```go
func TestClearAllRequestLogsRemovesRequestLogTablesOnly(t *testing.T) {
	dbPath := t.TempDir() + "/usage.db"
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	defer CloseDB()

	entry := LogEntry{
		Timestamp:   time.Now().UTC(),
		APIKey:      "sk-test",
		Model:       "gpt-5",
		TotalTokens: 12,
	}
	logID, err := InsertLog(entry)
	if err != nil {
		t.Fatalf("InsertLog() error = %v", err)
	}
	if _, err := getDB().Exec(
		`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content)
		 VALUES (?, ?, 'none', X'01', X'02')`,
		logID,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed request_log_content error = %v", err)
	}

	result, err := ClearAllRequestLogs()
	if err != nil {
		t.Fatalf("ClearAllRequestLogs() error = %v", err)
	}
	if result.DeletedLogs != 1 || result.DeletedContents != 1 {
		t.Fatalf("unexpected cleanup result: %#v", result)
	}

	var logsCount, contentCount int
	if err := getDB().QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&logsCount); err != nil {
		t.Fatalf("count request_logs error = %v", err)
	}
	if err := getDB().QueryRow("SELECT COUNT(*) FROM request_log_content").Scan(&contentCount); err != nil {
		t.Fatalf("count request_log_content error = %v", err)
	}
	if logsCount != 0 || contentCount != 0 {
		t.Fatalf("expected empty request log tables, got logs=%d content=%d", logsCount, contentCount)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/usage -run TestClearAllRequestLogsRemovesRequestLogTablesOnly -count=1`
Expected: FAIL because `ClearAllRequestLogs` does not exist yet.

- [ ] **Step 3: Write the minimal cleanup implementation**

```go
type ClearRequestLogsResult struct {
	DeletedLogs     int64 `json:"deleted_logs"`
	DeletedContents int64 `json:"deleted_contents"`
}

func ClearAllRequestLogs() (ClearRequestLogsResult, error) {
	db := getDB()
	if db == nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: database not initialised")
	}

	tx, err := db.Begin()
	if err != nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: begin clear request logs: %w", err)
	}
	defer rollbackTx(tx)

	contentResult, err := tx.Exec("DELETE FROM request_log_content")
	if err != nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_log_content: %w", err)
	}
	logResult, err := tx.Exec("DELETE FROM request_logs")
	if err != nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_logs: %w", err)
	}

	deletedContents, _ := contentResult.RowsAffected()
	deletedLogs, _ := logResult.RowsAffected()

	if err := tx.Commit(); err != nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: commit clear request logs: %w", err)
	}

	if _, err := db.Exec("VACUUM"); err != nil {
		log.Warnf("usage: vacuum after request log cleanup failed: %v", err)
	}

	return ClearRequestLogsResult{
		DeletedLogs:     deletedLogs,
		DeletedContents: deletedContents,
	}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/usage -run TestClearAllRequestLogsRemovesRequestLogTablesOnly -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/usage/usage_db.go internal/usage/usage_db_test.go
git commit -m "feat: add request log database cleanup helper"
```

### Task 2: Management API endpoint for request-log cleanup

**Files:**
- Modify: `internal/api/handlers/management/usage_logs_handler.go`
- Modify: `internal/api/server.go`
- Test: `internal/api/handlers/management/usage_logs_handler_test.go`

- [ ] **Step 1: Write the failing handler test**

```go
func TestDeleteUsageLogsClearsRequestLogDatabase(t *testing.T) {
	h := newTestManagementHandler(t)
	seedUsageLog(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/usage/logs", nil)

	h.DeleteUsageLogs(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), `"deleted_logs":1`) {
		t.Fatalf("body = %s, want deleted count", w.Body.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/handlers/management -run TestDeleteUsageLogsClearsRequestLogDatabase -count=1`
Expected: FAIL because the handler and route do not exist yet.

- [ ] **Step 3: Add the minimal handler and route**

```go
func (h *Handler) DeleteUsageLogs(c *gin.Context) {
	result, err := usage.ClearAllRequestLogs()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
```

```go
mgmt.DELETE("/usage/logs", s.mgmt.DeleteUsageLogs)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/api/handlers/management -run TestDeleteUsageLogsClearsRequestLogDatabase -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/handlers/management/usage_logs_handler.go internal/api/handlers/management/usage_logs_handler_test.go internal/api/server.go
git commit -m "feat: expose request log cleanup management endpoint"
```

### Task 3: Frontend API client and request logs page action

**Files:**
- Modify: `src/lib/http/apis/usage.ts`
- Modify: `src/modules/monitor/RequestLogsPage.tsx`
- Modify: `src/i18n/locales/en.json`
- Modify: `src/i18n/locales/zh-CN.json`
- Test: `src/modules/monitor/__tests__/RequestLogsPage.test.tsx`

- [ ] **Step 1: Write the failing page test**

```tsx
test("clears request-log database after confirmation", async () => {
  const user = userEvent.setup();
  mocks.getUsageLogs
    .mockResolvedValueOnce({
      items: [seedRow],
      total: 1,
      page: 1,
      size: 50,
      filters: { api_keys: [], api_key_names: {}, models: [], channels: [] },
      stats: { total: 1, success_rate: 100, total_tokens: 30, total_cost: 0.01 },
    })
    .mockResolvedValueOnce({
      items: [],
      total: 0,
      page: 1,
      size: 50,
      filters: { api_keys: [], api_key_names: {}, models: [], channels: [] },
      stats: { total: 0, success_rate: 0, total_tokens: 0, total_cost: 0 },
    });
  mocks.clearUsageLogs.mockResolvedValue({ deleted_logs: 1, deleted_contents: 1 });

  renderRequestLogsPage();
  await user.click(await screen.findByRole("button", { name: /Clear Database Logs/i }));
  await user.click(await screen.findByRole("button", { name: /Clear/i }));

  await waitFor(() => expect(mocks.clearUsageLogs).toHaveBeenCalledTimes(1));
  expect(await screen.findByText("No Data")).toBeInTheDocument();
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bun vitest run src/modules/monitor/__tests__/RequestLogsPage.test.tsx`
Expected: FAIL because the page has no cleanup button or API method yet.

- [ ] **Step 3: Add the minimal frontend implementation**

```ts
async clearUsageLogs(): Promise<{ deleted_logs: number; deleted_contents: number }> {
  return apiClient.delete("/usage/logs");
}
```

```tsx
<Button variant="danger" onClick={() => setConfirmClearOpen(true)}>
  {t("request_logs.clear_database_logs")}
</Button>
<ConfirmModal
  open={confirmClearOpen}
  title={t("request_logs.clear_database_logs")}
  description={t("request_logs.clear_database_logs_confirm")}
  confirmText={t("request_logs.clear_database_logs_confirm_button")}
  onClose={() => setConfirmClearOpen(false)}
  onConfirm={() => {
    setConfirmClearOpen(false);
    void handleClearDatabaseLogs();
  }}
/>
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `bun vitest run src/modules/monitor/__tests__/RequestLogsPage.test.tsx`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add src/lib/http/apis/usage.ts src/modules/monitor/RequestLogsPage.tsx src/modules/monitor/__tests__/RequestLogsPage.test.tsx src/i18n/locales/en.json src/i18n/locales/zh-CN.json
git commit -m "feat: add request log database cleanup action"
```

### Task 4: Final verification and integration

**Files:**
- Verify only the files above plus docs in `docs/superpowers/...`

- [ ] **Step 1: Run backend verification**

Run: `go test ./internal/usage ./internal/api/handlers/management -count=1`
Expected: PASS

- [ ] **Step 2: Run frontend verification**

Run: `bun vitest run src/modules/monitor/__tests__/RequestLogsPage.test.tsx src/modules/logs/__tests__/LogsPage.test.tsx`
Expected: PASS

- [ ] **Step 3: Run repo status checks**

Run: `git status --short`
Expected: no unrelated changes included in either feature branch

- [ ] **Step 4: Merge feature branches into local dev after verification**

```bash
git checkout dev
git merge --ff-only feature/issue-159-clear-request-log-db
git push origin dev
```

- [ ] **Step 5: Repeat merge flow for the second repo**

```bash
git checkout dev
git merge --ff-only feature/issue-159-clear-request-log-db
git push origin dev
```
