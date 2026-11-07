package enduser

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// UpdateUser 修改账号资料、状态与额度/权限，全部在一个事务里完成：账号行更新、
// 绑定模板时的快照复制、名下 Key 子额度的自动压低，最后一起提交并通知各节点刷新
// 鉴权快照。
func (s *Service) UpdateUser(ctx context.Context, actor identity.Principal, tenantID, userID string, username, displayName, password, status *string, accountQuotaPatch *QuotaPatch) (User, error) {
	if !actor.Has("end_users.write") && !actor.PlatformAdmin {
		return User{}, ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return User{}, err
	}
	if err := requireUUID(tenantID); err != nil {
		return User{}, err
	}
	if err := requireUUID(userID); err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadAccountQuotaTx(ctx, tx, tenantID, userID, true)
	if err != nil {
		return User{}, err
	}

	sets := make([]string, 0, 24)
	args := make([]any, 0, 28)
	if username != nil {
		base := NormalizeUsername(*username)
		if base == "" {
			return User{}, fmt.Errorf("%w: username required", ErrValidation)
		}
		uname, err := s.uniqueUsernameExcluding(ctx, tx, base, userID)
		if err != nil {
			return User{}, err
		}
		sets = append(sets, "username = ?", "username_normalized = ?")
		args = append(args, uname, NormalizeUsername(uname))
	}
	if displayName != nil {
		v := strings.TrimSpace(*displayName)
		if v == "" || len(v) > 128 {
			return User{}, fmt.Errorf("%w: invalid display_name", ErrValidation)
		}
		sets = append(sets, "display_name = ?")
		args = append(args, v)
	}
	if password != nil && strings.TrimSpace(*password) != "" {
		hash, err := HashPassword(*password)
		if err != nil {
			return User{}, err
		}
		sets = append(sets, "password_hash = ?", "must_change_password = false", "password_changed_at = CURRENT_TIMESTAMP",
			"failed_login_count = 0", "lock_stage = 0", "locked_until = NULL")
		args = append(args, hash)
	}
	if status != nil {
		st := strings.TrimSpace(*status)
		if st != "active" && st != "disabled" && st != "locked" {
			return User{}, fmt.Errorf("%w: invalid status", ErrValidation)
		}
		sets = append(sets, "status = ?")
		args = append(args, st)
		if st == "active" {
			sets = append(sets, "failed_login_count = 0", "lock_stage = 0", "locked_until = NULL")
		}
	}
	var bindProfile *sqlapikey.PermissionProfileRow
	if accountQuotaPatch != nil {
		plan, err := planAccountQuotaUpdate(ctx, tx, tenantID, current, accountQuotaPatch)
		if err != nil {
			return User{}, err
		}
		sets = append(sets, plan.sets...)
		args = append(args, plan.args...)
		bindProfile = plan.bindProfile
	}
	if len(sets) == 0 {
		_ = tx.Rollback()
		return s.GetUser(ctx, tenantID, userID)
	}

	sets = append(sets, "updated_at = CURRENT_TIMESTAMP", "version = version + 1")
	args = append(args, userID, tenantID)
	res, err := tx.ExecContext(ctx, `UPDATE end_users SET `+strings.Join(sets, ", ")+` WHERE id = ? AND tenant_id = ?`, args...)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	if bindProfile != nil {
		if err := copyProfileToAccountTx(ctx, tx, tenantID, userID, *bindProfile); err != nil {
			return User{}, err
		}
	}
	if password != nil && strings.TrimSpace(*password) != "" {
		if _, err = tx.ExecContext(ctx, `
			UPDATE end_user_sessions SET revoked_at = CURRENT_TIMESTAMP, revoke_reason = 'password_change'
			WHERE end_user_id = ? AND revoked_at IS NULL
		`, userID); err != nil {
			return User{}, err
		}
	}
	// Account freeze/activate is end_users.status only. Do not bulk-toggle api_keys.disabled:
	// that would wipe per-key admin disables on re-activate. Auth refuses non-active accounts.
	if status != nil && *status != "active" {
		if _, err = tx.ExecContext(ctx, `
				UPDATE end_user_sessions SET revoked_at = CURRENT_TIMESTAMP, revoke_reason = 'status_change'
				WHERE end_user_id = ? AND revoked_at IS NULL
			`, userID); err != nil {
			return User{}, err
		}
	}
	capped, err := capOwnedKeysToAccountTx(ctx, tx, tenantID, userID)
	if err != nil {
		return User{}, err
	}
	if err = commitAccountChange(ctx, tx, tenantID); err != nil {
		return User{}, err
	}
	updated, err := s.GetUser(ctx, tenantID, userID)
	if err != nil {
		return User{}, err
	}
	if loaded := usage.GetEndUserQuota(userID); loaded != nil {
		ApplyEffectiveQuota(&updated, usage.EffectiveEndUserQuota(*loaded))
	}
	updated.CappedKeys = quota.LegacyCappedKeys(capped)
	updated.CappedCells = capped
	if len(capped) > 0 {
		if audit := identity.Default(); audit != nil {
			audit.RecordAudit(ctx, identity.AuditEvent{
				TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID,
				Action: "end_user.period_quota.cap_keys", ResourceType: "end_user", ResourceID: userID, Result: "success",
				Changes: map[string]any{"capped-keys": updated.CappedKeys, "capped-cells": capped},
			})
		}
	}
	return updated, nil
}

type accountQuotaPlan struct {
	sets []string
	args []any
	// bindProfile 非 nil 时账号正在绑定（或换成）这个模板，写完补丁后用它覆盖账号行。
	bindProfile *sqlapikey.PermissionProfileRow
}

// planAccountQuotaUpdate 把额度/权限补丁翻译成 UPDATE 的 SET 子句。
//
// 绑定模板时模板是权威：模板拥有的额度格子、按模型规则与路由不能在账号上单独改，
// 只有累计消费（发给账号的额度）例外。旧版面板在绑定时会把模板的 daily-limit 等值
// 一起提交，这些旧字段照常接受，随后被模板快照覆盖为同一组值。
func planAccountQuotaUpdate(ctx context.Context, tx *sql.Tx, tenantID string, current accountQuotaRow, patch *QuotaPatch) (accountQuotaPlan, error) {
	var plan accountQuotaPlan
	resolvedPeriod, err := quotaPkgResolveLegacyDay(patch.DailySpendingLimit, patch.PeriodSpendingLimits)
	if err != nil {
		return plan, err
	}
	target := current.ProfileID
	if patch.PermissionProfileID != nil {
		target = strings.TrimSpace(*patch.PermissionProfileID)
	}
	bound := target != ""
	unbinding := !bound && current.ProfileID != ""
	if bound {
		if resolvedPeriod != nil {
			return plan, fmt.Errorf("%w: period limits are managed by permission profile %s", ErrValidation, target)
		}
		if touchesProfileOwnedCells(patch.QuotaLimits) || patch.ModelRules != nil || patch.ModelRoutes != nil {
			return plan, fmt.Errorf("%w: quota limits, model rules and model routes are managed by permission profile %s", ErrValidation, target)
		}
		if target != current.ProfileID {
			profile, err := sqlapikey.LoadPermissionProfileTx(ctx, tx, tenantID, target)
			if err != nil {
				return plan, err
			}
			if profile == nil {
				return plan, fmt.Errorf("%w: permission profile %s not found", ErrValidation, target)
			}
			plan.bindProfile = profile
		}
	}

	matrixPatch, err := mergeLegacyAccountLimits(patch, resolvedPeriod)
	if err != nil {
		return plan, err
	}
	nextMatrix, err := quota.ApplyMatrixPatch(current.matrix(), matrixPatch)
	if err != nil {
		return plan, fmt.Errorf("%w: quota-limits.%v", ErrValidation, err)
	}
	for _, col := range accountColumns {
		if matrixPatch.Get(col.metric, col.window) == nil {
			continue
		}
		plan.sets = append(plan.sets, col.column+" = ?")
		value := nextMatrix.Value(col.metric, col.window)
		if col.metric == quota.MetricRequests {
			plan.args = append(plan.args, int(value))
		} else {
			plan.args = append(plan.args, value)
		}
	}

	policyChanged := !matrixPatch.IsEmpty() || patch.ModelRules != nil || patch.ModelRoutes != nil || unbinding
	if policyChanged {
		next := current.Policy.Clone()
		next.Limits = quota.AccountColumnLayout.StripColumnCells(nextMatrix)
		if patch.ModelRules != nil {
			next.ModelRules = *patch.ModelRules
		}
		if patch.ModelRoutes != nil {
			next.ModelRoutes = *patch.ModelRoutes
		}
		if unbinding {
			// 与 allowed-* 的解绑清理一致（profile_unbind.go）：旧版面板看不到规则与路由，
			// 留在账号上就成了看不见、改不掉的限制。新版面板解绑时会显式带上要保留的值。
			if patch.ModelRules == nil {
				next.ModelRules = nil
			}
			if patch.ModelRoutes == nil {
				next.ModelRoutes = nil
			}
		}
		next, err = quota.NormalizeAccessPolicy(next, quota.AccountColumnLayout)
		if err != nil {
			return plan, err
		}
		if !bound {
			allowed := current.AllowedModels
			if patch.AllowedModels != nil {
				allowed = *patch.AllowedModels
			} else if unbinding {
				allowed = nil
			}
			if err := validatePolicyTargetsAllowed(next, allowed); err != nil {
				return plan, err
			}
		}
		plan.sets = append(plan.sets, "access_policy = ?")
		plan.args = append(plan.args, quota.EncodeAccessPolicy(next))
	}

	if patch.PermissionProfileID != nil {
		plan.sets, plan.args = appendPermissionProfileSets(plan.sets, plan.args, current.ProfileID, patch)
	}
	if patch.ConcurrencyLimit != nil {
		plan.sets = append(plan.sets, "concurrency_limit = ?")
		plan.args = append(plan.args, clampNonNegInt(*patch.ConcurrencyLimit))
	}
	if patch.RPMLimit != nil {
		plan.sets = append(plan.sets, "rpm_limit = ?")
		plan.args = append(plan.args, clampNonNegInt(*patch.RPMLimit))
	}
	if patch.TPMLimit != nil {
		plan.sets = append(plan.sets, "tpm_limit = ?")
		plan.args = append(plan.args, clampNonNegInt(*patch.TPMLimit))
	}
	if patch.AllowedModels != nil {
		plan.sets = append(plan.sets, "allowed_models = ?")
		plan.args = append(plan.args, encodeJSONStringList(*patch.AllowedModels))
	}
	if patch.AllowedChannels != nil {
		plan.sets = append(plan.sets, "allowed_channels = ?")
		plan.args = append(plan.args, encodeJSONStringList(*patch.AllowedChannels))
	}
	if patch.AllowedChannelGroups != nil {
		plan.sets = append(plan.sets, "allowed_channel_groups = ?")
		plan.args = append(plan.args, encodeJSONStringList(*patch.AllowedChannelGroups))
	}
	if patch.SystemPrompt != nil {
		plan.sets = append(plan.sets, "system_prompt = ?")
		plan.args = append(plan.args, *patch.SystemPrompt)
	}
	return plan, nil
}
