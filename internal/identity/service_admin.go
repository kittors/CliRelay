package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/ipaccess"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	log "github.com/sirupsen/logrus"
)

// AuditLog is the list/detail DTO for management audit records.
type AuditLog struct {
	ID               int64   `json:"id"`
	TenantID         *string `json:"tenant_id"`
	TenantName       string  `json:"tenant_name"`
	TenantSlug       string  `json:"tenant_slug"`
	ActorKind        string  `json:"actor_kind"`
	ActorUserID      *string `json:"actor_user_id"`
	ActorUsername    string  `json:"actor_username"`
	ActorDisplayName string  `json:"actor_display_name"`
	Action           string  `json:"action"`
	ResourceType     string  `json:"resource_type"`
	ResourceID       string  `json:"resource_id"`
	Result           string  `json:"result"`
	RequestID        string  `json:"request_id"`
	IPAddress        string  `json:"ip_address"`
	// Changes is only populated for detail views.
	Changes   map[string]any `json:"changes,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// AuditLogListResult is a page of audit logs.
type AuditLogListResult struct {
	Items []AuditLog `json:"items"`
	Total int64      `json:"total"`
	Page  int        `json:"page"`
	Size  int        `json:"size"`
}

const (
	defaultAuditLogPageSize = 50
	maxAuditLogPageSize     = 200
)

func (s *Service) RecordAudit(ctx context.Context, event AuditEvent) {
	if s == nil || s.db == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Domain callers often omit RequestID; pull from request context when present.
	if strings.TrimSpace(event.RequestID) == "" {
		event.RequestID = logging.GetRequestID(ctx)
	}
	var tenantID, actorUserID, actorSessionID any
	if event.TenantID != "" {
		tenantID = event.TenantID
	}
	if event.ActorUserID != "" {
		actorUserID = event.ActorUserID
	}
	if event.ActorSessionID != "" {
		actorSessionID = event.ActorSessionID
	}
	changes := event.Changes
	if changes == nil {
		changes = map[string]any{}
	}
	// Domain audit events may omit chain metadata; keep a minimal reconstructable trail.
	if _, ok := changes["call_chain"]; !ok {
		changes["call_chain"] = []map[string]any{
			{"step": 1, "layer": "service", "name": event.Action, "resource": event.ResourceType, "resource_id": event.ResourceID},
		}
	}
	if _, ok := changes["project_method"]; !ok {
		changes["project_method"] = map[string]any{
			"package":  "internal/identity",
			"method":   event.Action,
			"resource": event.ResourceType,
		}
	}
	changesJSON, err := json.Marshal(changes)
	if err != nil {
		changesJSON = []byte("{}")
	}
	// Source address comes from the request context rather than a parameter, so
	// every audit row gets provenance without each call site remembering to pass
	// it. An explicit event.IPAddress still wins for callers outside a request.
	ipAddress := event.IPAddress
	if ipAddress == "" {
		ipAddress = ipaccess.AuditAddress(ctx)
	}
	ctx = context.WithoutCancel(ctx)
	auditCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(auditCtx, `INSERT INTO audit_logs (tenant_id,actor_kind,actor_user_id,actor_session_id,action,resource_type,resource_id,result,request_id,changes,ip_address) VALUES (?,?,?,?,?,?,?,?,?,?::jsonb,?)`, tenantID, event.ActorKind, actorUserID, actorSessionID, event.Action, event.ResourceType, event.ResourceID, event.Result, event.RequestID, string(changesJSON), ipAddress); err != nil {
		log.WithError(err).WithFields(log.Fields{"action": event.Action, "resource_type": event.ResourceType, "resource_id": event.ResourceID}).Error("identity: record audit event")
	}
}

func normalizeAuditPage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = defaultAuditLogPageSize
	}
	if size > maxAuditLogPageSize {
		size = maxAuditLogPageSize
	}
	return page, size
}

func scanAuditLog(scanner interface {
	Scan(dest ...any) error
}, includeChanges bool) (AuditLog, error) {
	var item AuditLog
	var tenant, actor, tenantName, tenantSlug, actorUsername, actorDisplay sql.NullString
	var ipAddress sql.NullString
	var changesRaw []byte
	dest := []any{
		&item.ID, &tenant, &tenantName, &tenantSlug,
		&item.ActorKind, &actor, &actorUsername, &actorDisplay,
		&item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.RequestID, &ipAddress, &item.CreatedAt,
	}
	if includeChanges {
		dest = append(dest, &changesRaw)
	}
	if err := scanner.Scan(dest...); err != nil {
		return AuditLog{}, err
	}
	if tenant.Valid {
		item.TenantID = &tenant.String
	}
	if actor.Valid {
		item.ActorUserID = &actor.String
	}
	item.TenantName = tenantName.String
	item.TenantSlug = tenantSlug.String
	item.ActorUsername = actorUsername.String
	item.ActorDisplayName = actorDisplay.String
	item.IPAddress = ipAddress.String
	if includeChanges {
		item.Changes = map[string]any{}
		if len(changesRaw) > 0 {
			_ = json.Unmarshal(changesRaw, &item.Changes)
		}
	}
	return item, nil
}

const auditLogSelectBase = `
SELECT a.id, a.tenant_id, t.name, t.slug,
       a.actor_kind, a.actor_user_id, u.username, u.display_name,
       a.action, a.resource_type, a.resource_id, a.result, a.request_id, a.ip_address, a.created_at`

// ListAuditLogs returns a page of audit logs. Platform readers may see all tenants.
func (s *Service) ListAuditLogs(ctx context.Context, tenantID string, platform bool, page, size int) (AuditLogListResult, error) {
	page, size = normalizeAuditPage(page, size)
	where := ""
	args := []any{}
	if !platform {
		where = ` WHERE a.tenant_id = ?`
		args = append(args, tenantID)
	}
	var total int64
	countQuery := `SELECT COUNT(*) FROM audit_logs a` + where
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return AuditLogListResult{}, err
	}
	offset := (page - 1) * size
	listQuery := auditLogSelectBase + `
FROM audit_logs a
LEFT JOIN tenants t ON t.id = a.tenant_id
LEFT JOIN users u ON u.id = a.actor_user_id` + where + `
ORDER BY a.created_at DESC, a.id DESC
LIMIT ? OFFSET ?`
	listArgs := append(append([]any{}, args...), size, offset)
	rows, err := s.db.QueryContext(ctx, listQuery, listArgs...)
	if err != nil {
		return AuditLogListResult{}, err
	}
	defer rows.Close()
	items := make([]AuditLog, 0, size)
	for rows.Next() {
		item, scanErr := scanAuditLog(rows, false)
		if scanErr != nil {
			return AuditLogListResult{}, scanErr
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return AuditLogListResult{}, err
	}
	return AuditLogListResult{Items: items, Total: total, Page: page, Size: size}, nil
}

// GetAuditLog returns one audit log with full changes / call-chain payload.
func (s *Service) GetAuditLog(ctx context.Context, tenantID string, platform bool, id int64) (AuditLog, error) {
	query := auditLogSelectBase + `, a.changes
FROM audit_logs a
LEFT JOIN tenants t ON t.id = a.tenant_id
LEFT JOIN users u ON u.id = a.actor_user_id
WHERE a.id = ?`
	args := []any{id}
	if !platform {
		query += ` AND a.tenant_id = ?`
		args = append(args, tenantID)
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	item, err := scanAuditLog(row, true)
	if err != nil {
		return AuditLog{}, err
	}
	return item, nil
}

// DeleteAuditLog removes one audit log within the caller's visible scope.
func (s *Service) DeleteAuditLog(ctx context.Context, tenantID string, platform bool, id int64) error {
	query := `DELETE FROM audit_logs WHERE id = ?`
	args := []any{id}
	if !platform {
		query += ` AND tenant_id = ?`
		args = append(args, tenantID)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ClearAuditLogsResult reports how many audit rows were removed.
type ClearAuditLogsResult struct {
	Deleted int64 `json:"deleted"`
}

// ClearAuditLogs removes all audit logs visible to the caller.
// Platform readers clear every tenant; tenant callers only clear their own tenant.
func (s *Service) ClearAuditLogs(ctx context.Context, tenantID string, platform bool) (ClearAuditLogsResult, error) {
	var (
		result sql.Result
		err    error
	)
	if platform {
		result, err = s.db.ExecContext(ctx, `DELETE FROM audit_logs`)
	} else {
		result, err = s.db.ExecContext(ctx, `DELETE FROM audit_logs WHERE tenant_id = ?`, tenantID)
	}
	if err != nil {
		return ClearAuditLogsResult{}, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return ClearAuditLogsResult{}, err
	}
	return ClearAuditLogsResult{Deleted: deleted}, nil
}

func (s *Service) AssignUserRoles(ctx context.Context, actor Principal, tenantID, userID string, roleIDs []string) error {
	if !actor.Has("tenant.users.assign_roles") && !actor.Has("platform.users.manage") {
		return ErrPermissionDenied
	}
	if userID == SystemUserID {
		return ErrProtectedResource
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = ensureNotTenantAdmin(ctx, tx, tenantID, userID); err != nil {
		return err
	}
	if err = ensureRolesDelegable(ctx, tx, actor, tenantID, roleIDs); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id=?`, userID); err != nil {
		return err
	}
	for _, roleID := range roleIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_roles(user_id,role_id,created_by)VALUES(?,?,?)`, userID, roleID, actor.User.ID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET updated_at=now(),version=version+1 WHERE id=?`, userID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "user.roles.replace", ResourceType: "user", ResourceID: userID, Result: "success"})
	return nil
}

func (s *Service) ReplaceRoleUsers(ctx context.Context, actor Principal, tenantID, roleID string, userIDs []string, version int64) error {
	canManage := actor.Has("platform.users.manage") || (actor.Has("tenant.users.assign_roles") && actor.Has("tenant.roles.update"))
	if !canManage {
		return ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var roleTenant string
	var protected bool
	var currentVersion int64
	if err = tx.QueryRowContext(ctx, `SELECT tenant_id,system_protected,version FROM roles WHERE id=? FOR UPDATE`, roleID).Scan(&roleTenant, &protected, &currentVersion); err != nil {
		return err
	}
	if roleTenant != tenantID {
		return ErrTenantScope
	}
	if currentVersion != version {
		return ErrVersionConflict
	}
	if protected {
		return ErrProtectedResource
	}
	if err = ensureRolesDelegable(ctx, tx, actor, tenantID, []string{roleID}); err != nil {
		return err
	}

	selected := make(map[string]struct{}, len(userIDs))
	for _, userID := range userIDs {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			return fmt.Errorf("%w: user id is required", ErrValidation)
		}
		if userID == SystemUserID {
			return ErrProtectedResource
		}
		if _, exists := selected[userID]; exists {
			continue
		}
		var userTenant string
		if err = tx.QueryRowContext(ctx, `SELECT tenant_id FROM users WHERE id=?`, userID).Scan(&userTenant); err != nil {
			return err
		}
		if userTenant != tenantID {
			return ErrTenantScope
		}
		selected[userID] = struct{}{}
	}

	current := make(map[string]struct{})
	affected := make(map[string]struct{}, len(selected))
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM user_roles WHERE role_id=?`, roleID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var userID string
		if err = rows.Scan(&userID); err != nil {
			_ = rows.Close()
			return err
		}
		current[userID] = struct{}{}
		affected[userID] = struct{}{}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for userID := range current {
		if _, remainsAssigned := selected[userID]; remainsAssigned {
			continue
		}
		if err = ensureNotTenantAdmin(ctx, tx, tenantID, userID); err != nil {
			return err
		}
	}
	for userID := range selected {
		affected[userID] = struct{}{}
		if _, alreadyAssigned := current[userID]; alreadyAssigned {
			continue
		}
		if err = ensureNotTenantAdmin(ctx, tx, tenantID, userID); err != nil {
			return err
		}
	}

	if _, err = tx.ExecContext(ctx, `DELETE FROM user_roles WHERE role_id=?`, roleID); err != nil {
		return err
	}
	for userID := range selected {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_roles(user_id,role_id,created_by)VALUES(?,?,?)`, userID, roleID, actor.User.ID); err != nil {
			return err
		}
	}
	for userID := range affected {
		if _, err = tx.ExecContext(ctx, `UPDATE users SET updated_at=now(),version=version+1 WHERE id=?`, userID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE roles SET updated_at=now(),version=version+1 WHERE id=?`, roleID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "role.users.replace", ResourceType: "role", ResourceID: roleID, Result: "success"})
	return nil
}

func (s *Service) UpdateUserStatus(ctx context.Context, actor Principal, tenantID, userID, status string, version int64) (User, error) {
	if !actor.Has("tenant.users.update") && !actor.Has("platform.users.manage") {
		return User{}, ErrPermissionDenied
	}
	if userID == actor.User.ID || userID == SystemUserID {
		return User{}, ErrProtectedResource
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return User{}, err
	}
	if status != "active" && status != "disabled" && status != "locked" {
		return User{}, fmt.Errorf("%w: invalid user status", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if status != "active" {
		if err = ensureNotLastTenantAdmin(ctx, tx, tenantID, userID); err != nil {
			return User{}, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET status=?,updated_at=now(),version=version+1 WHERE id=? AND tenant_id=? AND version=?`, status, userID, tenantID, version)
	if err != nil {
		return User{}, err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return User{}, ErrVersionConflict
	}
	if status != "active" {
		if _, err = tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at=now(),revoke_reason='account_status_changed' WHERE user_id=? AND revoked_at IS NULL`, userID); err != nil {
			return User{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "user.status.update", ResourceType: "user", ResourceID: userID, Result: "success"})
	users, err := s.ListUsers(ctx, tenantID)
	if err != nil {
		return User{}, err
	}
	for _, user := range users {
		if user.ID == userID {
			return user, nil
		}
	}
	return User{}, sql.ErrNoRows
}

func (s *Service) DeleteUser(ctx context.Context, actor Principal, tenantID, userID string) error {
	if !actor.Has("tenant.users.delete") && !actor.Has("platform.users.manage") {
		return ErrPermissionDenied
	}
	if userID == actor.User.ID || userID == SystemUserID {
		return ErrProtectedResource
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = ensureNotTenantAdmin(ctx, tx, tenantID, userID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=? AND tenant_id=?`, userID, tenantID)
	if err != nil {
		return err
	}
	if count, _ := res.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "user.delete", ResourceType: "user", ResourceID: userID, Result: "success"})
	return nil
}

func (s *Service) DeleteRole(ctx context.Context, actor Principal, tenantID, roleID string) error {
	if !actor.Has("tenant.roles.delete") && !actor.Has("platform.roles.manage") {
		return ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var protected bool
	if err = tx.QueryRowContext(ctx, `SELECT system_protected FROM roles WHERE id=? AND tenant_id=? FOR UPDATE`, roleID, tenantID).Scan(&protected); err != nil {
		return err
	}
	if protected {
		return ErrProtectedResource
	}
	var assigned int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_roles WHERE role_id=?`, roleID).Scan(&assigned); err != nil {
		return err
	}
	if assigned > 0 {
		return ErrProtectedResource
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM roles WHERE id=? AND tenant_id=?`, roleID, tenantID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "role.delete", ResourceType: "role", ResourceID: roleID, Result: "success"})
	return nil
}

func (s *Service) DeleteTenant(ctx context.Context, actor Principal, tenantID string, version int64) (Tenant, error) {
	if !actor.Has("platform.tenants.update") {
		return Tenant{}, ErrPermissionDenied
	}
	if tenantID == SystemTenantID {
		return Tenant{}, ErrProtectedResource
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tenant{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE tenants SET status='disabled',updated_at=now(),version=version+1 WHERE id=? AND version=?`, tenantID, version)
	if err != nil {
		return Tenant{}, err
	}
	if count, _ := res.RowsAffected(); count == 0 {
		return Tenant{}, ErrVersionConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at=now(),revoke_reason='tenant_disabled' WHERE user_id IN (SELECT id FROM users WHERE tenant_id=?) AND revoked_at IS NULL`, tenantID); err != nil {
		return Tenant{}, err
	}
	if err = commitTenantChange(ctx, tx, tenantID); err != nil {
		return Tenant{}, err
	}
	s.InvalidateTenant(tenantID)
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "tenant.disable", ResourceType: "tenant", ResourceID: tenantID, Result: "success"})
	return s.GetTenant(ctx, tenantID)
}

func ensureNotTenantAdmin(ctx context.Context, tx *sql.Tx, tenantID, userID string) error {
	var userTenant string
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM users WHERE id=? FOR UPDATE`, userID).Scan(&userTenant); err != nil {
		return err
	}
	if userTenant != tenantID {
		return ErrTenantScope
	}
	var tenantAdmin bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=? AND r.tenant_id=? AND r.code='tenant_admin' AND r.scope='tenant' AND r.system_protected=true)`, userID, tenantID).Scan(&tenantAdmin); err != nil {
		return err
	}
	if tenantAdmin {
		return ErrProtectedResource
	}
	return nil
}

func ensureNotLastTenantAdmin(ctx context.Context, tx *sql.Tx, tenantID, userID string) error {
	var targetIsAdmin bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=? AND r.tenant_id=? AND r.system_protected=true)`, userID, tenantID).Scan(&targetIsAdmin); err != nil {
		return err
	}
	if !targetIsAdmin {
		return nil
	}
	var activeAdmins int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT u.id) FROM users u JOIN user_roles ur ON ur.user_id=u.id JOIN roles r ON r.id=ur.role_id WHERE u.tenant_id=? AND u.status='active' AND r.system_protected=true`, tenantID).Scan(&activeAdmins); err != nil {
		return err
	}
	if activeAdmins <= 1 {
		return ErrProtectedResource
	}
	return nil
}

func (s *Service) ValidateTenantAccess(ctx context.Context, tenantID string) error {
	_, err := s.TenantAccessDeadline(ctx, tenantID)
	return err
}

func (s *Service) TenantAccessDeadline(ctx context.Context, tenantID string) (*time.Time, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, ErrTenantScope
	}
	tenant, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if err = validateTenant(tenant, time.Now()); err != nil {
		return nil, err
	}
	if tenant.Type == "system" || tenant.ExpiresAt == nil {
		return nil, nil
	}
	deadline := tenant.ExpiresAt.UTC()
	return &deadline, nil
}
