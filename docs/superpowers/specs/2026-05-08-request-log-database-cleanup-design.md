# Request Log Database Cleanup Design

**Problem**

Issue [#159](https://github.com/kittors/CliRelay/issues/159) asks for a simple way to clear the growing SQLite request-log data shown in the management panel. The current panel already exposes:

- `请求日志 / Request Logs` for SQLite-backed request history and message content.
- `日志查询 / Logs` for file-based live logs and error log downloads.

Those are different storage systems, so the cleanup entry must make the scope obvious and avoid implying that API keys, model configs, routing data, or other database-backed management records will be deleted.

**Decision**

Add a dangerous action entry to the `Request Logs` page, not to `Logs` or `System Monitor`.

Why this is the best fit:

- The page already represents `request_logs` and `request_log_content`, so the cleanup target matches the user’s mental model.
- It avoids mixing SQLite request history cleanup with file log cleanup.
- It keeps the blast radius narrow: only request-log history is cleared, while management configuration stored in SQLite remains untouched.

**Scope**

Add a one-click management action that:

- Deletes all rows from `request_logs`.
- Deletes all rows from `request_log_content`.
- Optionally runs a SQLite `VACUUM`/checkpoint style reclaim step in the same backend operation when safe.
- Refreshes the `Request Logs` page so totals, filters, rows, and storage indicators immediately drop to zero/empty.

Out of scope:

- Deleting API keys, model configs, channel groups, provider settings, permission templates, or any other business data tables.
- Clearing file-based runtime logs under the log directory.
- Adding new YAML config knobs.

**Backend Design**

Add a management endpoint under the existing usage-log namespace:

- `DELETE /v0/management/usage/logs`

Behavior:

- Call a new usage-layer helper to clear only request-log tables.
- Return JSON with deleted counts so the UI can show an informative success toast.
- Keep the handler behind the existing management auth middleware.

Usage-layer helper:

- Add `ClearAllRequestLogs() (ClearRequestLogsResult, error)` in `internal/usage/usage_db.go`.
- Execute the delete inside a transaction.
- Delete `request_log_content` explicitly, then `request_logs`.
- Run a `VACUUM` only after the transaction commits, using the same database handle.
- Log the number of removed rows for auditability.

Suggested response shape:

```json
{
  "deleted_logs": 12345,
  "deleted_contents": 12345
}
```

**Frontend Design**

Add the entry to the title action area of `src/modules/monitor/RequestLogsPage.tsx`.

UI behavior:

- Render a danger-style button such as `Clear Database Logs`.
- Open a `ConfirmModal` with explicit copy:
  - This clears SQLite request-log history and message bodies.
  - This does not delete API keys or other management configuration.
  - This action cannot be undone.
- On confirm, call the new `usageApi.clearUsageLogs()` method.
- Clear current table state and refetch page 1 so the table, stats, and filters match the backend.
- Show a success toast with the deleted count when available.

**Testing Strategy**

Backend:

- Add a usage-layer test proving `ClearAllRequestLogs()` removes both tables while leaving unrelated DB-backed data intact.
- Add a handler test proving `DELETE /usage/logs` returns `200` with counts and empties subsequent log queries.

Frontend:

- Add a `RequestLogsPage` test that opens the modal, confirms the action, calls the API, and shows the updated empty state.
- Keep `LogsPage` behavior unchanged to guard against storage-scope confusion.

**Risk Controls**

- The button lives only on `Request Logs`, not globally.
- Confirmation copy states the exact deletion scope.
- Backend helper is limited to request-log tables.
- No production deployment or restart is performed as part of this task.
