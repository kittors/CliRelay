package identity

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const MenuManagementCode = "system.menus"

type Menu struct {
	Code            string `json:"code"`
	ParentCode      string `json:"parent_code"`
	Type            string `json:"type"`
	Path            string `json:"path"`
	Component       string `json:"component"`
	LinkURL         string `json:"link_url"`
	LabelKey        string `json:"label_key"`
	Title           string `json:"title"`
	Icon            string `json:"icon"`
	PermissionCode  string `json:"permission_code"`
	SortOrder       int    `json:"sort_order"`
	Visible         bool   `json:"visible"`
	Enabled         bool   `json:"enabled"`
	BadgeType       string `json:"badge_type"`
	BadgeContent    string `json:"badge_content"`
	HideMenu        bool   `json:"hide_menu"`
	SystemProtected bool   `json:"system_protected"`
	Version         int64  `json:"version"`
}

type MenuSeed struct {
	Code           string
	ParentCode     string
	Type           string
	Path           string
	Component      string
	LabelKey       string
	Icon           string
	PermissionCode string
	SortOrder      int
}

type MenuInput struct {
	Code           string `json:"code"`
	ParentCode     string `json:"parent_code"`
	Type           string `json:"type"`
	Path           string `json:"path"`
	Component      string `json:"component"`
	LinkURL        string `json:"link_url"`
	LabelKey       string `json:"label_key"`
	Title          string `json:"title"`
	Icon           string `json:"icon"`
	PermissionCode string `json:"permission_code"`
	SortOrder      int    `json:"sort_order"`
	Visible        bool   `json:"visible"`
	Enabled        bool   `json:"enabled"`
	BadgeType      string `json:"badge_type"`
	BadgeContent   string `json:"badge_content"`
	HideMenu       bool   `json:"hide_menu"`
	Version        int64  `json:"version"`
}

// MenuCatalog is the system menu seed.
// Directories carry a route prefix (path) and Layout component so nested children form
// secondary routes under that prefix (e.g. /runtime/monitor under /runtime).
//
// Information architecture (operator-facing):
// 仪表盘 — overview
// 运行观测 — live health, request traces, runtime logs
// 接入与凭证 — upstream providers, AI OAuth accounts, client API keys, key profiles
// 模型与调度 — model plaza, catalog, image models, routing groups, outbound proxies
// 组织与权限 — tenants, users, roles, audit
// 系统设置 — global config, menu management
// 系统信息 — host/runtime status (top-level leaf, pinned last)
var MenuCatalog = []MenuSeed{
	{Code: "dashboard", Type: "menu", Path: "/dashboard", Component: "dashboard", LabelKey: "shell.nav_dashboard", Icon: "layout-dashboard", PermissionCode: "dashboard.read", SortOrder: 10},
	{Code: "group.runtime", Type: "directory", Path: "/runtime", Component: "Layout", LabelKey: "shell.nav_group_runtime", Icon: "activity", SortOrder: 20},
	{Code: "group.access", Type: "directory", Path: "/access", Component: "Layout", LabelKey: "shell.nav_group_access", Icon: "bot", SortOrder: 30},
	{Code: "group.models", Type: "directory", Path: "/models", Component: "Layout", LabelKey: "shell.nav_group_models", Icon: "layers", SortOrder: 40},
	{Code: "group.governance", Type: "directory", Path: "/governance", Component: "Layout", LabelKey: "shell.nav_group_governance", Icon: "users-round", SortOrder: 50},
	{Code: "group.system", Type: "directory", Path: "/system", Component: "Layout", LabelKey: "shell.nav_group_system", Icon: "settings", SortOrder: 60},
	// Top-level leaf after all groups. Stable code kept for role/menu bindings; path stays under /runtime.
	{Code: "runtime.system", Type: "menu", Path: "/runtime/system", Component: "system", LabelKey: "shell.nav_system", Icon: "info", PermissionCode: "system.status.read", SortOrder: 70},
	// Runtime / observability
	{Code: "runtime.monitor", ParentCode: "group.runtime", Type: "menu", Path: "/runtime/monitor", Component: "monitor", LabelKey: "shell.nav_monitor", Icon: "activity", PermissionCode: "monitor.read", SortOrder: 10},
	{Code: "runtime.request-logs", ParentCode: "group.runtime", Type: "menu", Path: "/runtime/request-logs", Component: "request-logs", LabelKey: "shell.nav_request_logs", Icon: "scroll-text", PermissionCode: "request_logs.read", SortOrder: 20},
	{Code: "runtime.logs", ParentCode: "group.runtime", Type: "menu", Path: "/runtime/logs", Component: "logs", LabelKey: "shell.nav_logs", Icon: "file-text", PermissionCode: "system.logs.read", SortOrder: 30},
	// Access & credentials (upstream AI + client keys)
	{Code: "access.providers", ParentCode: "group.access", Type: "menu", Path: "/access/ai-providers", Component: "providers", LabelKey: "shell.nav_ai_providers", Icon: "bot", PermissionCode: "providers.read", SortOrder: 10},
	// Stable code kept for role/menu bindings; path lives under /access as AI OAuth accounts.
	{Code: "system.account-security", ParentCode: "group.access", Type: "menu", Path: "/access/ai-accounts", Component: "account-security", LabelKey: "shell.nav_ai_accounts", Icon: "key-round", PermissionCode: "auth_files.read", SortOrder: 20},
	{Code: "access.api-keys", ParentCode: "group.access", Type: "menu", Path: "/access/api-keys", Component: "api-keys", LabelKey: "shell.nav_api_keys", Icon: "sparkles", PermissionCode: "api_keys.read", SortOrder: 30},
	{Code: "access.end-users", ParentCode: "group.access", Type: "menu", Path: "/access/end-users", Component: "end-users", LabelKey: "shell.nav_end_users", Icon: "user-round", PermissionCode: "end_users.read", SortOrder: 35},
	// Stable code kept; API Key permission profiles belong with client credentials.
	{Code: "system.api-key-permissions", ParentCode: "group.access", Type: "menu", Path: "/access/api-key-permissions", Component: "api-key-permissions", LabelKey: "shell.nav_api_key_permissions", Icon: "shield-check", PermissionCode: "api_key_profiles.read", SortOrder: 40},
	// Tenant-scoped: matches /ccswitch-import-configs API auth (routing.read/write).
	// Must not use platform system.config.read, or ordinary tenants never see this menu.
	{Code: "access.ccswitch", ParentCode: "group.access", Type: "menu", Path: "/access/ccswitch-import-settings", Component: "ccswitch-import-settings", LabelKey: "shell.nav_ccswitch_import_settings", Icon: "arrow-down-to-line", PermissionCode: "routing.read", SortOrder: 50},
	// Models & routing
	// Tenant-visible available models (same permission as former System Info model list).
	{Code: "models.plaza", ParentCode: "group.models", Type: "menu", Path: "/models/plaza", Component: "model-plaza", LabelKey: "shell.nav_model_plaza", Icon: "store", PermissionCode: "system.status.read", SortOrder: 5},
	{Code: "models.catalog", ParentCode: "group.models", Type: "menu", Path: "/models/catalog", Component: "models", LabelKey: "shell.nav_models", Icon: "cpu", PermissionCode: "models.read", SortOrder: 10},
	{Code: "models.image-generation", ParentCode: "group.models", Type: "menu", Path: "/models/image-generation", Component: "image-generation", LabelKey: "shell.nav_image_generation", Icon: "image", PermissionCode: "system.config.read", SortOrder: 20},
	{Code: "models.channel-groups", ParentCode: "group.models", Type: "menu", Path: "/models/channel-groups", Component: "channel-groups", LabelKey: "shell.nav_channel_groups", Icon: "layers", PermissionCode: "routing.read", SortOrder: 30},
	{Code: "models.proxies", ParentCode: "group.models", Type: "menu", Path: "/models/proxies", Component: "proxies", LabelKey: "shell.nav_proxies", Icon: "network", PermissionCode: "proxies.read", SortOrder: 40},
	// Organization
	{Code: "governance.tenants", ParentCode: "group.governance", Type: "menu", Path: "/governance/tenants", Component: "tenants", LabelKey: "shell.nav_tenants", Icon: "building-2", PermissionCode: "platform.tenants.read", SortOrder: 10},
	{Code: "governance.users", ParentCode: "group.governance", Type: "menu", Path: "/governance/users", Component: "users", LabelKey: "shell.nav_users", Icon: "user-round", PermissionCode: "tenant.users.read", SortOrder: 20},
	{Code: "governance.roles", ParentCode: "group.governance", Type: "menu", Path: "/governance/roles", Component: "roles", LabelKey: "shell.nav_roles", Icon: "shield-check", PermissionCode: "tenant.roles.read", SortOrder: 30},
	{Code: "governance.audit", ParentCode: "group.governance", Type: "menu", Path: "/governance/audit-logs", Component: "audit-logs", LabelKey: "shell.nav_audit_logs", Icon: "file-text", PermissionCode: "tenant.audit.read", SortOrder: 40},
	// System settings only
	{Code: "system.config", ParentCode: "group.system", Type: "menu", Path: "/system/config", Component: "config", LabelKey: "shell.nav_config", Icon: "settings", PermissionCode: "system.config.read", SortOrder: 10},
	{Code: MenuManagementCode, ParentCode: "group.system", Type: "menu", Path: "/system/menu-management", Component: "menu-management", LabelKey: "shell.nav_menu_management", Icon: "menu", PermissionCode: "platform.menus.read", SortOrder: 20},
}

const menuSelectSQL = `SELECT code,parent_code,menu_type,path,component,link_url,label_key,title,icon,permission_code,sort_order,visible,enabled,badge_type,badge_content,hide_menu,system_protected,version FROM menus`

func seedMenus(ctx context.Context, tx *sql.Tx) error {
	for _, menu := range MenuCatalog {
		var parent any
		if menu.ParentCode != "" {
			parent = menu.ParentCode
		}
		var permission any
		if menu.PermissionCode != "" {
			permission = menu.PermissionCode
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO menus (code,parent_code,menu_type,path,component,label_key,icon,permission_code,sort_order,system_protected)
			VALUES (?,?,?,?,?,?,?,?,?,true)
			ON CONFLICT (code) DO UPDATE SET
			  parent_code=EXCLUDED.parent_code, menu_type=EXCLUDED.menu_type, path=EXCLUDED.path,
			  component=EXCLUDED.component, label_key=EXCLUDED.label_key, icon=EXCLUDED.icon,
			  permission_code=EXCLUDED.permission_code, sort_order=EXCLUDED.sort_order,
			  system_protected=true, updated_at=now()
		`, menu.Code, parent, menu.Type, menu.Path, menu.Component, menu.LabelKey, menu.Icon, permission, menu.SortOrder); err != nil {
			return fmt.Errorf("identity: seed menu %s: %w", menu.Code, err)
		}
	}
	return nil
}

func scanMenu(scanner interface {
	Scan(dest ...any) error
}) (Menu, error) {
	var menu Menu
	var parent, permission sql.NullString
	if err := scanner.Scan(
		&menu.Code, &parent, &menu.Type, &menu.Path, &menu.Component, &menu.LinkURL, &menu.LabelKey, &menu.Title,
		&menu.Icon, &permission, &menu.SortOrder, &menu.Visible, &menu.Enabled, &menu.BadgeType, &menu.BadgeContent,
		&menu.HideMenu, &menu.SystemProtected, &menu.Version,
	); err != nil {
		return Menu{}, err
	}
	if parent.Valid {
		menu.ParentCode = parent.String
	}
	if permission.Valid {
		menu.PermissionCode = permission.String
	}
	return menu, nil
}

func (s *Service) ListMenus(ctx context.Context) ([]Menu, error) {
	rows, err := s.db.QueryContext(ctx, menuSelectSQL+` ORDER BY sort_order,code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	menus := make([]Menu, 0)
	for rows.Next() {
		menu, err := scanMenu(rows)
		if err != nil {
			return nil, err
		}
		menus = append(menus, menu)
	}
	return menus, rows.Err()
}

func (s *Service) GetMenu(ctx context.Context, code string) (Menu, error) {
	row := s.db.QueryRowContext(ctx, menuSelectSQL+` WHERE code=?`, code)
	menu, err := scanMenu(row)
	if err != nil {
		return Menu{}, err
	}
	return menu, nil
}

func isNavLeafType(menuType string) bool {
	switch menuType {
	case "menu", "embed", "link":
		return true
	default:
		return false
	}
}

func (s *Service) ListPrincipalMenus(ctx context.Context, principal Principal) ([]Menu, error) {
	menus, err := s.ListMenus(ctx)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]Menu, len(menus))
	included := make(map[string]bool, len(menus))
	for _, menu := range menus {
		byCode[menu.Code] = menu
		if (isNavLeafType(menu.Type) || menu.Type == "button") && (menu.PermissionCode == "" || principal.Has(menu.PermissionCode)) {
			included[menu.Code] = true
		}
	}
	for code := range included {
		for parent := byCode[code].ParentCode; parent != ""; parent = byCode[parent].ParentCode {
			included[parent] = true
		}
	}
	result := make([]Menu, 0, len(included))
	for _, menu := range menus {
		if included[menu.Code] {
			result = append(result, menu)
		}
	}
	return result, nil
}

func normalizeMenuInput(input MenuInput, create bool) (MenuInput, error) {
	input.Code = strings.TrimSpace(input.Code)
	input.ParentCode = strings.TrimSpace(input.ParentCode)
	input.Type = strings.TrimSpace(input.Type)
	input.Path = strings.TrimSpace(input.Path)
	input.Component = strings.TrimSpace(input.Component)
	input.LinkURL = strings.TrimSpace(input.LinkURL)
	input.LabelKey = strings.TrimSpace(input.LabelKey)
	input.Title = strings.TrimSpace(input.Title)
	input.Icon = strings.TrimSpace(input.Icon)
	input.PermissionCode = strings.TrimSpace(input.PermissionCode)
	input.BadgeType = strings.TrimSpace(input.BadgeType)
	input.BadgeContent = strings.TrimSpace(input.BadgeContent)
	if input.Code == "" || input.LabelKey == "" {
		return MenuInput{}, fmt.Errorf("%w: code and label_key required", ErrValidation)
	}
	if input.SortOrder < 0 || input.SortOrder > 10000 {
		return MenuInput{}, fmt.Errorf("%w: invalid sort_order", ErrValidation)
	}
	switch input.Type {
	case "directory":
		// Directory is a route-prefix node: path is the secondary-route base (e.g. /runtime).
		// Component defaults to Layout so the admin table shows a real binding, not blank.
		if input.Path == "" {
			return MenuInput{}, fmt.Errorf("%w: directory path required", ErrValidation)
		}
		if !strings.HasPrefix(input.Path, "/") {
			return MenuInput{}, fmt.Errorf("%w: path must start with /", ErrValidation)
		}
		if input.Component == "" {
			input.Component = "Layout"
		}
		input.LinkURL = ""
	case "menu":
		if input.Path == "" {
			return MenuInput{}, fmt.Errorf("%w: menu path required", ErrValidation)
		}
		if !strings.HasPrefix(input.Path, "/") {
			return MenuInput{}, fmt.Errorf("%w: path must start with /", ErrValidation)
		}
	case "button":
		input.Path = ""
		input.Component = ""
		input.LinkURL = ""
	case "embed", "link":
		if input.Path == "" || input.LinkURL == "" {
			return MenuInput{}, fmt.Errorf("%w: path and link_url required", ErrValidation)
		}
		if !strings.HasPrefix(input.Path, "/") {
			return MenuInput{}, fmt.Errorf("%w: path must start with /", ErrValidation)
		}
		input.Component = ""
	default:
		return MenuInput{}, fmt.Errorf("%w: invalid menu type", ErrValidation)
	}
	if create && input.Version != 0 {
		input.Version = 0
	}
	return input, nil
}

func (s *Service) validateMenuRelations(ctx context.Context, input MenuInput, existingCode string) error {
	if input.ParentCode != "" {
		if input.ParentCode == input.Code {
			return fmt.Errorf("%w: parent cannot be self", ErrValidation)
		}
		parent, err := s.GetMenu(ctx, input.ParentCode)
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: parent menu not found", ErrValidation)
			}
			return err
		}
		if parent.Type == "button" || parent.Type == "link" {
			return fmt.Errorf("%w: invalid parent type", ErrValidation)
		}
		// Nested page/embed/link under a directory must use a secondary path under the parent prefix.
		if parent.Type == "directory" && parent.Path != "" &&
			(input.Type == "menu" || input.Type == "embed" || input.Type == "link") &&
			input.Path != "" {
			prefix := strings.TrimRight(parent.Path, "/")
			if input.Path != prefix && !strings.HasPrefix(input.Path, prefix+"/") {
				return fmt.Errorf("%w: child path must be nested under parent path %s", ErrValidation, prefix)
			}
		}
		// prevent cycles when reparenting
		if existingCode != "" {
			for cursor := parent; cursor.ParentCode != ""; {
				if cursor.ParentCode == existingCode {
					return fmt.Errorf("%w: cyclic parent", ErrValidation)
				}
				next, err := s.GetMenu(ctx, cursor.ParentCode)
				if err != nil {
					return err
				}
				cursor = next
			}
		}
	}
	if input.PermissionCode != "" {
		var code string
		if err := s.db.QueryRowContext(ctx, `SELECT code FROM permissions WHERE code=?`, input.PermissionCode).Scan(&code); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: permission not found", ErrValidation)
			}
			return err
		}
	}
	return nil
}

func (s *Service) CreateMenu(ctx context.Context, actor Principal, input MenuInput) (Menu, error) {
	if !actor.Has("platform.menus.update") {
		return Menu{}, ErrPermissionDenied
	}
	input, err := normalizeMenuInput(input, true)
	if err != nil {
		return Menu{}, err
	}
	if err := s.validateMenuRelations(ctx, input, ""); err != nil {
		return Menu{}, err
	}
	var parent any
	if input.ParentCode != "" {
		parent = input.ParentCode
	}
	var permission any
	if input.PermissionCode != "" {
		permission = input.PermissionCode
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO menus (
		  code,parent_code,menu_type,path,component,link_url,label_key,title,icon,permission_code,
		  sort_order,visible,enabled,badge_type,badge_content,hide_menu,system_protected
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,false)
	`, input.Code, parent, input.Type, input.Path, input.Component, input.LinkURL, input.LabelKey, input.Title,
		input.Icon, permission, input.SortOrder, input.Visible, input.Enabled, input.BadgeType, input.BadgeContent, input.HideMenu); err != nil {
		return Menu{}, err
	}
	menu, err := s.GetMenu(ctx, input.Code)
	if err != nil {
		return Menu{}, err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: actor.EffectiveTenant.ID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "menu.create", ResourceType: "menu", ResourceID: input.Code, Result: "success"})
	return menu, nil
}

func (s *Service) UpdateMenu(ctx context.Context, actor Principal, code string, input MenuInput) (Menu, error) {
	if !actor.Has("platform.menus.update") {
		return Menu{}, ErrPermissionDenied
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return Menu{}, fmt.Errorf("%w: code required", ErrValidation)
	}
	existing, err := s.GetMenu(ctx, code)
	if err != nil {
		if err == sql.ErrNoRows {
			return Menu{}, ErrVersionConflict
		}
		return Menu{}, err
	}
	input.Code = code
	input, err = normalizeMenuInput(input, false)
	if err != nil {
		return Menu{}, err
	}
	if (code == MenuManagementCode || code == "group.system") && (!input.Visible || !input.Enabled) {
		return Menu{}, ErrProtectedResource
	}
	if existing.SystemProtected {
		// keep seed identity stable; allow display/order/meta edits only for protected
		input.Type = existing.Type
		if input.Path == "" {
			input.Path = existing.Path
		}
		if input.Component == "" {
			input.Component = existing.Component
		}
		if input.PermissionCode == "" {
			input.PermissionCode = existing.PermissionCode
		}
		if input.ParentCode == "" {
			input.ParentCode = existing.ParentCode
		}
	}
	if err := s.validateMenuRelations(ctx, input, code); err != nil {
		return Menu{}, err
	}
	var parent any
	if input.ParentCode != "" {
		parent = input.ParentCode
	}
	var permission any
	if input.PermissionCode != "" {
		permission = input.PermissionCode
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE menus SET
		  parent_code=?, menu_type=?, path=?, component=?, link_url=?, label_key=?, title=?, icon=?,
		  permission_code=?, sort_order=?, visible=?, enabled=?, badge_type=?, badge_content=?, hide_menu=?,
		  updated_at=now(), version=version+1
		WHERE code=? AND version=?
	`, parent, input.Type, input.Path, input.Component, input.LinkURL, input.LabelKey, input.Title, input.Icon,
		permission, input.SortOrder, input.Visible, input.Enabled, input.BadgeType, input.BadgeContent, input.HideMenu,
		code, input.Version)
	if err != nil {
		return Menu{}, err
	}
	if count, _ := res.RowsAffected(); count == 0 {
		return Menu{}, ErrVersionConflict
	}
	menu, err := s.GetMenu(ctx, code)
	if err != nil {
		return Menu{}, err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: actor.EffectiveTenant.ID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "menu.update", ResourceType: "menu", ResourceID: code, Result: "success"})
	return menu, nil
}

func (s *Service) DeleteMenu(ctx context.Context, actor Principal, code string, version int64) error {
	if !actor.Has("platform.menus.update") {
		return ErrPermissionDenied
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("%w: code required", ErrValidation)
	}
	existing, err := s.GetMenu(ctx, code)
	if err != nil {
		if err == sql.ErrNoRows {
			return ErrVersionConflict
		}
		return err
	}
	if existing.SystemProtected || code == MenuManagementCode || code == "group.system" {
		return ErrProtectedResource
	}
	if existing.Version != version {
		return ErrVersionConflict
	}
	var childCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM menus WHERE parent_code=?`, code).Scan(&childCount); err != nil {
		return err
	}
	if childCount > 0 {
		return fmt.Errorf("%w: menu has children", ErrValidation)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM menus WHERE code=? AND version=? AND system_protected=false`, code, version)
	if err != nil {
		return err
	}
	if count, _ := res.RowsAffected(); count == 0 {
		return ErrVersionConflict
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: actor.EffectiveTenant.ID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "menu.delete", ResourceType: "menu", ResourceID: code, Result: "success"})
	return nil
}
