package enduser

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
)

// MaxBatchProfileBinding 限制一次批量绑定的账号数，避免一个事务锁住整张账号表。
const MaxBatchProfileBinding = 500

// BindResult 是批量绑定权限模板的结果。
type BindResult struct {
	Updated     int                `json:"updated"`
	CappedKeys  []quota.CappedKey  `json:"capped-keys,omitempty"`
	CappedCells []quota.CappedCell `json:"capped-cells,omitempty"`
}

// BindPermissionProfile 把一批账号绑定到同一个权限模板。
//
// 模板是成批管理账号权限的入口：每个账号写入模板的快照（与单个绑定一致，见
// copyProfileToAccountTx），名下 Key 的子额度压到新天花板之内，全部在一个事务里，
// 任何一个账号失败则整批不生效。批量操作只做绑定：解绑会按账号各自清理模板留下的
// 访问范围，适合在单个账号的编辑弹窗里确认后进行。
func (s *Service) BindPermissionProfile(ctx context.Context, actor identity.Principal, tenantID string, ids []string, profileID string) (BindResult, error) {
	var result BindResult
	if !actor.Has("end_users.write") && !actor.PlatformAdmin {
		return result, ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return result, err
	}
	if err := requireUUID(tenantID); err != nil {
		return result, err
	}
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return result, fmt.Errorf("%w: permission-profile-id is required", ErrValidation)
	}
	seen := make(map[string]struct{}, len(ids))
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := requireUUID(id); err != nil {
			return result, fmt.Errorf("%w: invalid end user id %q", ErrValidation, id)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	if len(clean) == 0 {
		return result, fmt.Errorf("%w: ids must name at least one end user", ErrValidation)
	}
	if len(clean) > MaxBatchProfileBinding {
		return result, fmt.Errorf("%w: at most %d end users per batch", ErrValidation, MaxBatchProfileBinding)
	}
	// 固定加锁顺序，两个并发的批量操作不会互相死锁。
	sort.Strings(clean)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	profile, err := sqlapikey.LoadPermissionProfileTx(ctx, tx, tenantID, profileID)
	if err != nil {
		return result, err
	}
	if profile == nil {
		return result, fmt.Errorf("%w: permission profile %s not found", ErrValidation, profileID)
	}
	for _, id := range clean {
		if _, err := loadAccountQuotaTx(ctx, tx, tenantID, id, true); err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE end_users SET permission_profile_id = ?, updated_at = CURRENT_TIMESTAMP, version = version + 1
			WHERE id = ? AND tenant_id = ?`, profileID, id, tenantID); err != nil {
			return result, err
		}
		if err := copyProfileToAccountTx(ctx, tx, tenantID, id, *profile); err != nil {
			return result, err
		}
		capped, err := capOwnedKeysToAccountTx(ctx, tx, tenantID, id)
		if err != nil {
			return result, err
		}
		result.CappedCells = append(result.CappedCells, capped...)
		result.Updated++
	}
	if err := commitAccountChange(ctx, tx, tenantID); err != nil {
		return result, err
	}
	result.CappedKeys = quota.LegacyCappedKeys(result.CappedCells)
	if audit := identity.Default(); audit != nil {
		audit.RecordAudit(ctx, identity.AuditEvent{
			TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID,
			Action: "end_user.permission_profile.bind_batch", ResourceType: "end_user", Result: "success",
			Changes: map[string]any{"ids": clean, "permission-profile-id": profileID, "capped-cells": result.CappedCells},
		})
	}
	return result, nil
}
