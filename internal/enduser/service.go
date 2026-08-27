package enduser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials        = errors.New("invalid credentials")
	ErrAccountDisabled           = errors.New("account disabled")
	ErrAccountLocked             = errors.New("account locked")
	ErrLoginCooldowned           = errors.New("login cooldown")
	ErrMustChangePassword        = errors.New("must change password")
	ErrSessionExpired            = errors.New("session expired")
	ErrSessionRevoked            = errors.New("session revoked")
	ErrPermissionDenied          = errors.New("permission denied")
	ErrTenantScope               = errors.New("tenant scope forbidden")
	ErrTenantSuspended           = errors.New("tenant suspended")
	ErrTenantExpired             = errors.New("tenant expired")
	ErrValidation                = errors.New("validation failed")
	ErrDuplicateKeyName          = errors.New("duplicate key name")
	ErrLastKey                   = errors.New("cannot delete last api key")
	ErrNotFound                  = errors.New("not found")
	ErrFiveHourProjectionWarming = errors.New("five hour quota projection warming")
	ErrPeriodDayLegacyConflict   = errors.New("period day legacy conflict")
)

const (
	defaultAccessTTL  = 12 * time.Hour
	defaultRefreshTTL = 30 * 24 * time.Hour
	loginFailStep     = 5
	accessPrefix      = "cpt_"
	refreshPrefix     = "cpr_usr_"
	keyAlphabet       = "abcdefghijklmnopqrstuvwxyz0123456789"
	// bcrypt hash of a never-used password for timing equalization
	dummyPasswordHash = "$2a$10$7EqJtq98hPqEX7fNZaFWoO5fKvR2qv4V5BfQWqHkVq3VP7N5x5V7e"
)

type Service struct {
	db *sql.DB
}

var (
	defaultMu      sync.RWMutex
	defaultService *Service
)

func SetDefault(s *Service) {
	defaultMu.Lock()
	defaultService = s
	defaultMu.Unlock()
}

func Default() *Service {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultService
}

func NewService(db *sql.DB) *Service {
	if db != nil {
		_ = usage.EnsureEndUserQuotaColumns(db)
		for _, column := range []string{"five_hour_spending_limit", "weekly_spending_limit", "monthly_spending_limit"} {
			if _, err := db.Exec("ALTER TABLE api_keys ADD COLUMN " + column + " REAL NOT NULL DEFAULT 0"); err != nil {
				message := strings.ToLower(err.Error())
				if !strings.Contains(message, "duplicate") && !strings.Contains(message, "no such table") {
					log.Warnf("enduser: migrate api_keys.%s: %v", column, err)
				}
			}
		}
	}
	return &Service{db: db}
}

func NormalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", fmt.Errorf("%w: password must contain at least 8 characters", ErrValidation)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func randomPrefixedToken(prefix string) (plain, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	plain = prefix + base64.RawURLEncoding.EncodeToString(raw)
	return plain, tokenHash(plain), nil
}

func randomPassword() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func GenerateAPIKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("sk-")
	for _, v := range raw {
		b.WriteByte(keyAlphabet[int(v)%len(keyAlphabet)])
	}
	return b.String(), nil
}

func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 10 {
		return key
	}
	return key[:5] + strings.Repeat("•", 16) + key[len(key)-3:]
}

// UsernameFromDisplay builds a login username from a display name / key name.
// Han characters map through a small common-surname/name table to pinyin; remaining
// non-ASCII becomes a short stable hash segment. Collisions get numeric suffixes by caller.
func UsernameFromDisplay(display string) string {
	display = strings.TrimSpace(display)
	if display == "" {
		return randomUserSlug()
	}
	var b strings.Builder
	for _, r := range strings.ToLower(display) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteByte('_')
		default:
			if py, ok := hanPinyin[r]; ok {
				b.WriteString(py)
			} else if r > unicode.MaxASCII {
				// stable fragment for unmapped han / symbols
				sum := sha256.Sum256([]byte(string(r)))
				b.WriteString(hex.EncodeToString(sum[:2]))
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if out == "" {
		sum := sha256.Sum256([]byte(display))
		return "u_" + hex.EncodeToString(sum[:6])
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// Common Chinese surname/given-name syllables used in existing key display names.
// Enough for migration nicknames without a full pinyin dictionary dependency.
var hanPinyin = map[rune]string{
	'陈': "chen", '龙': "long", '袁': "yuan", '蔚': "wei", '唐': "tang", '承': "cheng",
	'震': "zhen", '张': "zhang", '军': "jun", '宝': "bao", '郭': "guo", '学': "xue",
	'书': "shu", '吴': "wu", '俊': "jun", '杰': "jie", '方': "fang", '勇': "yong",
	'石': "shi", '雷': "lei", '波': "bo", '韩': "han", '飞': "fei", '胡': "hu",
	'才': "cai", '亮': "liang", '王': "wang", '李': "li", '刘': "liu", '赵': "zhao",
	'黄': "huang", '周': "zhou", '徐': "xu", '孙': "sun", '马': "ma", '朱': "zhu",
	'林': "lin", '何': "he", '高': "gao", '梁': "liang", '郑': "zheng", '罗': "luo",
	'宋': "song", '谢': "xie", '曹': "cao", '许': "xu", '邓': "deng", '萧': "xiao",
	'冯': "feng", '曾': "zeng", '程': "cheng", '蔡': "cai", '彭': "peng", '潘': "pan",
	'于': "yu", '董': "dong", '余': "yu", '苏': "su", '叶': "ye", '吕': "lv",
	'魏': "wei", '蒋': "jiang", '田': "tian", '杜': "du", '丁': "ding", '沈': "shen",
	'姜': "jiang", '范': "fan", '江': "jiang", '傅': "fu", '钟': "zhong", '卢': "lu",
	'汪': "wang", '戴': "dai", '崔': "cui", '任': "ren", '陆': "lu", '廖': "liao",
	'姚': "yao", '金': "jin", '邱': "qiu", '夏': "xia", '谭': "tan", '韦': "wei",
	'贾': "jia", '邹': "zou", '熊': "xiong", '孟': "meng", '秦': "qin", '阎': "yan",
	'薛': "xue", '侯': "hou", '白': "bai", '段': "duan", '郝': "hao", '孔': "kong",
	'邵': "shao", '史': "shi", '毛': "mao", '常': "chang", '万': "wan", '顾': "gu",
	'赖': "lai", '武': "wu", '康': "kang", '贺': "he", '严': "yan", '尹': "yin",
	'钱': "qian", '施': "shi", '牛': "niu", '洪': "hong", '龚': "gong", '伟': "wei",
	'强': "qiang", '敏': "min", '静': "jing", '丽': "li", '娜': "na", '芳': "fang",
	'燕': "yan", '红': "hong", '华': "hua", '明': "ming", '平': "ping", '刚': "gang",
	'超': "chao", '辉': "hui", '鹏': "peng", '涛': "tao", '浩': "hao", '宇': "yu",
	'轩': "xuan", '博': "bo", '文': "wen", '斌': "bin", '洋': "yang", '鑫': "xin",
	'磊': "lei", '峰': "feng", '凯': "kai", '健': "jian", '建': "jian", '国': "guo",
	'东': "dong", '海': "hai", '云': "yun", '成': "cheng", '志': "zhi", '永': "yong",
	'新': "xin", '生': "sheng", '兵': "bing",
}

func randomUserSlug() string {
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	return "user_" + hex.EncodeToString(raw)
}

func (s *Service) ensureTenantActive(ctx context.Context, tenantID string) error {
	var status, tenantType string
	var expires sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT status, type, expires_at FROM tenants WHERE id = ?`, tenantID).Scan(&status, &tenantType, &expires)
	if err != nil {
		return err
	}
	if status != "active" {
		return ErrTenantSuspended
	}
	if tenantType != "system" && expires.Valid && !expires.Time.After(time.Now()) {
		return ErrTenantExpired
	}
	return nil
}

func (s *Service) tenantTTL(ctx context.Context, tenantID string) (access, refresh time.Duration) {
	access, refresh = defaultAccessTTL, defaultRefreshTTL
	if s == nil || s.db == nil {
		return
	}
	var a, r sql.NullInt64
	_ = s.db.QueryRowContext(ctx, `SELECT access_token_ttl_seconds, refresh_token_ttl_seconds FROM tenants WHERE id = ?`, tenantID).Scan(&a, &r)
	if a.Valid && a.Int64 > 0 {
		access = time.Duration(a.Int64) * time.Second
	}
	if r.Valid && r.Int64 > 0 {
		refresh = time.Duration(r.Int64) * time.Second
	}
	return
}

func ensureActorTenantScope(actor identity.Principal, tenantID string) error {
	if actor.PlatformAdmin || tenantID == actor.EffectiveTenant.ID {
		return nil
	}
	return ErrTenantScope
}

func requireUUID(id string) error {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return ErrNotFound
	}
	return nil
}

func scanUser(scanner interface{ Scan(dest ...any) error }) (User, error) {
	var u User
	var lastLogin, lockedUntil sql.NullTime
	var modelsJSON, channelsJSON, groupsJSON string
	err := scanner.Scan(
		&u.ID, &u.TenantID, &u.Username, &u.DisplayName, &u.Status,
		&u.MustChangePassword, &lastLogin, &u.FailedLoginCount, &u.LockStage, &lockedUntil,
		&u.CreatedAt, &u.UpdatedAt, &u.Version,
		&u.PermissionProfileID, &u.DailyLimit, &u.TotalQuota, &u.SpendingLimit, &u.DailySpendingLimit,
		&u.PeriodSpendingLimits.FiveHour, &u.PeriodSpendingLimits.Week, &u.PeriodSpendingLimits.Month, &u.ConcurrencyLimit, &u.RPMLimit, &u.TPMLimit,
		&modelsJSON, &channelsJSON, &groupsJSON, &u.SystemPrompt,
	)
	if err != nil {
		return u, err
	}
	if lastLogin.Valid {
		t := lastLogin.Time
		u.LastLoginAt = &t
	}
	if lockedUntil.Valid {
		t := lockedUntil.Time
		u.LockedUntil = &t
	}
	u.PeriodSpendingLimits.Day = u.DailySpendingLimit
	u.AllowedModels = decodeJSONStringList(modelsJSON)
	u.AllowedChannels = decodeJSONStringList(channelsJSON)
	u.AllowedChannelGroups = decodeJSONStringList(groupsJSON)
	return u, nil
}

func decodeJSONStringList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func encodeJSONStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func clampNonNegInt(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func clampNonNegFloat(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

const userSelect = `SELECT id, tenant_id, username, display_name, status, must_change_password,
	last_login_at, failed_login_count, lock_stage, locked_until, created_at, updated_at, version,
	COALESCE(permission_profile_id, ''), COALESCE(daily_limit, 0), COALESCE(total_quota, 0),
	COALESCE(spending_limit, 0), COALESCE(daily_spending_limit, 0),
	COALESCE(five_hour_spending_limit, 0), COALESCE(weekly_spending_limit, 0), COALESCE(monthly_spending_limit, 0),
	COALESCE(concurrency_limit, 0), COALESCE(rpm_limit, 0), COALESCE(tpm_limit, 0),
	COALESCE(allowed_models, '[]'), COALESCE(allowed_channels, '[]'), COALESCE(allowed_channel_groups, '[]'),
	COALESCE(system_prompt, '')
	FROM end_users`

func (s *Service) GetUser(ctx context.Context, tenantID, userID string) (User, error) {
	if err := requireUUID(tenantID); err != nil {
		return User{}, err
	}
	if err := requireUUID(userID); err != nil {
		return User{}, err
	}
	row := s.db.QueryRowContext(ctx, userSelect+` WHERE tenant_id = ? AND id = ?`, tenantID, userID)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Service) ListUsers(ctx context.Context, actor identity.Principal, tenantID string) ([]User, error) {
	if !actor.Has("end_users.read") && !actor.PlatformAdmin {
		return nil, ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return nil, err
	}
	if err := requireUUID(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, userSelect+` WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts, err := s.apiKeyCountsByUser(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].APIKeyCount = counts[out[i].ID]
	}
	return out, nil
}

func (s *Service) apiKeyCountsByUser(ctx context.Context, tenantID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT end_user_id, COUNT(*) FROM api_keys
		WHERE tenant_id = ? AND end_user_id IS NOT NULL AND disabled = 0
		GROUP BY end_user_id
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Service) uniqueUsername(ctx context.Context, tx *sql.Tx, _ string, base string) (string, error) {
	return s.uniqueUsernameExcluding(ctx, tx, base, "")
}

func (s *Service) uniqueUsernameExcluding(ctx context.Context, tx *sql.Tx, base, excludeUserID string) (string, error) {
	// Global unique usernames (portal login is not tenant-scoped).
	base = NormalizeUsername(base)
	if base == "" {
		base = randomUserSlug()
	}
	candidate := base
	for i := 2; i < 1000; i++ {
		var n int
		var err error
		if excludeUserID != "" {
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM end_users WHERE username_normalized = ? AND id <> ?`, candidate, excludeUserID).Scan(&n)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM end_users WHERE username_normalized = ?`, candidate).Scan(&n)
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s%d", base, i)
	}
	return "", fmt.Errorf("%w: cannot allocate unique username", ErrValidation)
}

func (s *Service) insertDefaultKey(ctx context.Context, tx *sql.Tx, tenantID, endUserID, name string) (APIKey, string, error) {
	var plain string
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		plain, err = GenerateAPIKey()
		if err != nil {
			return APIKey{}, "", err
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE key = ?`, plain).Scan(&n); err != nil {
			return APIKey{}, "", err
		}
		if n == 0 {
			break
		}
		if attempt == 7 {
			return APIKey{}, "", fmt.Errorf("failed to generate unique api key")
		}
	}
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	if name == "" {
		name = "default"
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO api_keys (key, id, name, disabled, end_user_id, is_default, tenant_id, created_at, updated_at,
			permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit,
			concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt)
		VALUES (?, ?, ?, 0, ?, true, ?, ?, ?,
			'', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, '[]', '[]', '[]', '')
	`, plain, id, name, endUserID, tenantID, now, now); err != nil {
		return APIKey{}, "", err
	}
	return APIKey{
		ID: id, TenantID: tenantID, EndUserID: endUserID, Key: plain, KeyMasked: MaskAPIKey(plain),
		Name: name, IsDefault: true, CreatedAt: now, UpdatedAt: now,
	}, plain, nil
}

func (s *Service) CreateUser(ctx context.Context, actor identity.Principal, tenantID, username, displayName, password string) (CreateUserResult, error) {
	var result CreateUserResult
	if !actor.Has("end_users.write") && !actor.PlatformAdmin {
		return result, ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return result, err
	}
	if err := requireUUID(tenantID); err != nil {
		return result, err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || len(displayName) > 128 {
		return result, fmt.Errorf("%w: display_name required", ErrValidation)
	}
	generated := ""
	if strings.TrimSpace(password) == "" {
		var err error
		generated, err = randomPassword()
		if err != nil {
			return result, err
		}
		password = generated
	}
	hash, err := HashPassword(password)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	base := strings.TrimSpace(username)
	if base == "" {
		base = UsernameFromDisplay(displayName)
	}
	uname, err := s.uniqueUsername(ctx, tx, tenantID, base)
	if err != nil {
		return result, err
	}
	userID := uuid.NewString()
	mustChange := true // admin-created portal users always change once
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, userID, tenantID, uname, NormalizeUsername(uname), displayName, hash, mustChange); err != nil {
		return result, err
	}
	key, plain, err := s.insertDefaultKey(ctx, tx, tenantID, userID, displayName)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	u, err := s.GetUser(ctx, tenantID, userID)
	if err != nil {
		return result, err
	}
	key.Key = plain
	result = CreateUserResult{User: u, GeneratedPassword: generated, DefaultAPIKey: &key}
	return result, nil
}

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
	sets := make([]string, 0, 20)
	args := make([]any, 0, 24)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var currentProfileID string
	profileQuery := `SELECT COALESCE(permission_profile_id,'') FROM end_users WHERE id = ? AND tenant_id = ? FOR UPDATE`
	if err := tx.QueryRowContext(ctx, profileQuery, userID, tenantID).Scan(&currentProfileID); err != nil {
		if err = tx.QueryRowContext(ctx, strings.TrimSuffix(profileQuery, " FOR UPDATE"), userID, tenantID).Scan(&currentProfileID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return User{}, ErrNotFound
			}
			return User{}, err
		}
	}
	var resolvedPeriodPatch *quota.PeriodSpendingLimitsPatch
	if accountQuotaPatch != nil {
		resolvedPeriodPatch, err = quotaPkgResolveLegacyDay(accountQuotaPatch.DailySpendingLimit, accountQuotaPatch.PeriodSpendingLimits)
		if err != nil {
			return User{}, err
		}
		targetProfileID := currentProfileID
		if accountQuotaPatch.PermissionProfileID != nil {
			targetProfileID = strings.TrimSpace(*accountQuotaPatch.PermissionProfileID)
		}
		if resolvedPeriodPatch != nil && targetProfileID != "" {
			return User{}, fmt.Errorf("%w: period limits are managed by permission profile %s", ErrValidation, targetProfileID)
		}
	}

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
	if accountQuotaPatch != nil {
		if accountQuotaPatch.PermissionProfileID != nil {
			sets, args = appendPermissionProfileSets(sets, args, currentProfileID, accountQuotaPatch)
		}
		if accountQuotaPatch.DailyLimit != nil {
			sets = append(sets, "daily_limit = ?")
			args = append(args, clampNonNegInt(*accountQuotaPatch.DailyLimit))
		}
		if accountQuotaPatch.TotalQuota != nil {
			sets = append(sets, "total_quota = ?")
			args = append(args, clampNonNegInt(*accountQuotaPatch.TotalQuota))
		}
		if accountQuotaPatch.SpendingLimit != nil {
			sets = append(sets, "spending_limit = ?")
			args = append(args, clampNonNegFloat(*accountQuotaPatch.SpendingLimit))
		}
		if resolvedPeriodPatch != nil {
			if resolvedPeriodPatch.FiveHour != nil {
				if *resolvedPeriodPatch.FiveHour > 0 && !usage.FiveHourQuotaProjectionReady() {
					return User{}, ErrFiveHourProjectionWarming
				}
				sets = append(sets, "five_hour_spending_limit = ?")
				args = append(args, *resolvedPeriodPatch.FiveHour)
			}
			if resolvedPeriodPatch.Day != nil {
				sets = append(sets, "daily_spending_limit = ?")
				args = append(args, *resolvedPeriodPatch.Day)
			}
			if resolvedPeriodPatch.Week != nil {
				sets = append(sets, "weekly_spending_limit = ?")
				args = append(args, *resolvedPeriodPatch.Week)
			}
			if resolvedPeriodPatch.Month != nil {
				sets = append(sets, "monthly_spending_limit = ?")
				args = append(args, *resolvedPeriodPatch.Month)
			}
		}
		if accountQuotaPatch.ConcurrencyLimit != nil {
			sets = append(sets, "concurrency_limit = ?")
			args = append(args, clampNonNegInt(*accountQuotaPatch.ConcurrencyLimit))
		}
		if accountQuotaPatch.RPMLimit != nil {
			sets = append(sets, "rpm_limit = ?")
			args = append(args, clampNonNegInt(*accountQuotaPatch.RPMLimit))
		}
		if accountQuotaPatch.TPMLimit != nil {
			sets = append(sets, "tpm_limit = ?")
			args = append(args, clampNonNegInt(*accountQuotaPatch.TPMLimit))
		}
		if accountQuotaPatch.AllowedModels != nil {
			sets = append(sets, "allowed_models = ?")
			args = append(args, encodeJSONStringList(*accountQuotaPatch.AllowedModels))
		}
		if accountQuotaPatch.AllowedChannels != nil {
			sets = append(sets, "allowed_channels = ?")
			args = append(args, encodeJSONStringList(*accountQuotaPatch.AllowedChannels))
		}
		if accountQuotaPatch.AllowedChannelGroups != nil {
			sets = append(sets, "allowed_channel_groups = ?")
			args = append(args, encodeJSONStringList(*accountQuotaPatch.AllowedChannelGroups))
		}
		if accountQuotaPatch.SystemPrompt != nil {
			sets = append(sets, "system_prompt = ?")
			args = append(args, *accountQuotaPatch.SystemPrompt)
		}
	}
	if len(sets) == 0 {
		_ = tx.Rollback()
		return s.GetUser(ctx, tenantID, userID)
	}

	sets = append(sets, "updated_at = CURRENT_TIMESTAMP", "version = version + 1")
	args = append(args, userID, tenantID)
	q := `UPDATE end_users SET ` + strings.Join(sets, ", ") + ` WHERE id = ? AND tenant_id = ?`
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
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
	capped, err := capOwnedKeyPeriodLimitsTx(ctx, tx, tenantID, userID)
	if err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	updated, err := s.GetUser(ctx, tenantID, userID)
	if err != nil {
		return User{}, err
	}
	if loaded := usage.GetEndUserQuota(userID); loaded != nil {
		effective := usage.EffectiveEndUserQuota(*loaded)
		updated.DailySpendingLimit = effective.DailySpendingLimit
		updated.PeriodSpendingLimits = effective.PeriodSpendingLimits
	}
	updated.CappedKeys = capped
	if len(capped) > 0 {
		if audit := identity.Default(); audit != nil {
			audit.RecordAudit(ctx, identity.AuditEvent{
				TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID,
				Action: "end_user.period_quota.cap_keys", ResourceType: "end_user", ResourceID: userID, Result: "success",
				Changes: map[string]any{"capped-keys": capped},
			})
		}
	}
	return updated, nil
}

func (s *Service) ResetPassword(ctx context.Context, actor identity.Principal, tenantID, userID, password string) (string, error) {
	if !actor.Has("end_users.write") && !actor.PlatformAdmin {
		return "", ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return "", err
	}
	if err := requireUUID(tenantID); err != nil {
		return "", err
	}
	if err := requireUUID(userID); err != nil {
		return "", err
	}
	generated := ""
	if strings.TrimSpace(password) == "" {
		var err error
		generated, err = randomPassword()
		if err != nil {
			return "", err
		}
		password = generated
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE end_users SET password_hash = ?, must_change_password = true, password_changed_at = CURRENT_TIMESTAMP,
			failed_login_count = 0, lock_stage = 0, locked_until = NULL,
			status = CASE WHEN status = 'locked' THEN 'active' ELSE status END,
			updated_at = CURRENT_TIMESTAMP, version = version + 1
		WHERE id = ? AND tenant_id = ?
	`, hash, userID, tenantID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE end_user_sessions SET revoked_at = CURRENT_TIMESTAMP, revoke_reason = 'password_reset'
		WHERE end_user_id = ? AND revoked_at IS NULL
	`, userID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return generated, nil
}

func (s *Service) DeleteUser(ctx context.Context, actor identity.Principal, tenantID, userID string) error {
	if !actor.Has("end_users.write") && !actor.PlatformAdmin {
		return ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return err
	}
	if err := requireUUID(tenantID); err != nil {
		return err
	}
	if err := requireUUID(userID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Disable keys so API access ends with the account; then unbind (not resurrected by one-shot backfill).
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err = tx.ExecContext(ctx, `
		UPDATE api_keys SET disabled = 1, is_default = false, end_user_id = NULL, updated_at = ?
		WHERE tenant_id = ? AND end_user_id = ?
	`, now, tenantID, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE end_user_sessions SET revoked_at = CURRENT_TIMESTAMP, revoke_reason = 'user_deleted'
		WHERE end_user_id = ? AND revoked_at IS NULL
	`, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM end_user_daily_spending_resets WHERE tenant_id = ? AND end_user_id = ?
	`, tenantID, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM end_user_daily_spending_reset_events WHERE tenant_id = ? AND end_user_id = ?
	`, tenantID, userID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM end_users WHERE id = ? AND tenant_id = ?`, userID, tenantID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ResolveOwnedKeySecret returns the plaintext key for a key owned by this end user.
// Used by the portal to drive usage lookup without re-pasting after login.
func (s *Service) ResolveOwnedKeySecret(ctx context.Context, tenantID, endUserID, keyID string) (string, error) {
	var secret string
	err := s.db.QueryRowContext(ctx, `
		SELECT key FROM api_keys
		WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND disabled = 0
	`, tenantID, endUserID, keyID).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return secret, nil
}

func (s *Service) ListKeys(ctx context.Context, tenantID, endUserID string) ([]APIKey, error) {
	if err := requireUUID(tenantID); err != nil {
		return nil, err
	}
	if err := requireUUID(endUserID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, end_user_id, key, name, disabled, COALESCE(is_default, false),
			COALESCE(created_at, ''), COALESCE(updated_at, ''),
			COALESCE(daily_spending_limit, 0), COALESCE(five_hour_spending_limit, 0), COALESCE(weekly_spending_limit, 0), COALESCE(monthly_spending_limit, 0)
		FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0
		ORDER BY is_default DESC, created_at ASC
	`, tenantID, endUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIKey, 0)
	ids := make([]string, 0)
	for rows.Next() {
		var k APIKey
		var ownerID sql.NullString
		var disabledInt int
		var isDefault bool
		if err := rows.Scan(&k.ID, &k.TenantID, &ownerID, &k.Key, &k.Name, &disabledInt, &isDefault, &k.CreatedAt, &k.UpdatedAt, &k.DailySpendingLimit, &k.PeriodSpendingLimits.FiveHour, &k.PeriodSpendingLimits.Week, &k.PeriodSpendingLimits.Month); err != nil {
			return nil, err
		}
		if ownerID.Valid {
			k.EndUserID = ownerID.String
		}
		k.Disabled = disabledInt != 0
		k.PeriodSpendingLimits.Day = k.DailySpendingLimit
		k.IsDefault = isDefault
		k.KeyMasked = MaskAPIKey(k.Key)
		k.Key = "" // never list full secret
		out = append(out, k)
		ids = append(ids, k.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if usage.RuntimeDB() == nil {
		return out, nil
	}
	usedByID, err := usage.QueryPeriodSpendingByAPIKeyIDsForTenant(tenantID, ids)
	if err != nil {
		return nil, err
	}
	resetCounts, err := usage.ListDailySpendingResetEventCounts(tenantID, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		used := usedByID[out[i].ID]
		out[i].DailySpendingUsed = used.Day
		out[i].LifetimeSpendingUsed = used.Lifetime
		out[i].PeriodSpending = quota.BuildPeriodSpending(out[i].PeriodSpendingLimits, used)
		out[i].DailySpendingResetCount = resetCounts[out[i].ID]
	}
	return out, nil
}

func (s *Service) CreateKey(ctx context.Context, tenantID, endUserID, name string) (CreateKeyResult, error) {
	return s.CreateKeyWithPeriodLimits(ctx, tenantID, endUserID, name, nil)
}

func (s *Service) SetDefaultKey(ctx context.Context, tenantID, endUserID, keyID string) error {
	if err := requireUUID(tenantID); err != nil {
		return err
	}
	if err := requireUUID(endUserID); err != nil {
		return err
	}
	if err := requireUUID(keyID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND disabled = 0`, tenantID, endUserID, keyID).Scan(&n); err != nil || n == 0 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET is_default = false, updated_at = ? WHERE tenant_id = ? AND end_user_id = ?`, time.Now().UTC().Format(time.RFC3339), tenantID, endUserID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET is_default = true, updated_at = ? WHERE tenant_id = ? AND end_user_id = ? AND id = ?`, time.Now().UTC().Format(time.RFC3339), tenantID, endUserID, keyID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) UpdateKeyName(ctx context.Context, tenantID, endUserID, keyID, name string) error {
	return s.UpdateKey(ctx, tenantID, endUserID, keyID, &name, nil)
}

// ensureUniqueKeyName rejects case-insensitive name collisions within one end user.
// excludeKeyID is empty on create; on rename pass the current key id so it can keep its name.
func ensureUniqueKeyName(ctx context.Context, q queryRower, tenantID, endUserID, name, excludeKeyID string) error {
	var exists int
	var err error
	if excludeKeyID == "" {
		err = q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM api_keys
			WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0 AND LOWER(name) = LOWER(?)
		`, tenantID, endUserID, name).Scan(&exists)
	} else {
		err = q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM api_keys
			WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0 AND LOWER(name) = LOWER(?) AND id != ?
		`, tenantID, endUserID, name, excludeKeyID).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if exists > 0 {
		return fmt.Errorf("%w: key name %q already exists", ErrDuplicateKeyName, name)
	}
	return nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Service) RotateKey(ctx context.Context, tenantID, endUserID, keyID string) (CreateKeyResult, error) {
	var result CreateKeyResult
	if err := requireUUID(tenantID); err != nil {
		return result, err
	}
	if err := requireUUID(endUserID); err != nil {
		return result, err
	}
	if err := requireUUID(keyID); err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldSecret string
	if err = tx.QueryRowContext(ctx, `SELECT key FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND disabled = 0`, tenantID, endUserID, keyID).Scan(&oldSecret); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrNotFound
		}
		return result, err
	}
	var plain string
	for attempt := 0; attempt < 8; attempt++ {
		plain, err = GenerateAPIKey()
		if err != nil {
			return result, err
		}
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE key = ?`, plain).Scan(&exists); err != nil {
			return result, err
		}
		if exists == 0 {
			break
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET key = ?, updated_at = ? WHERE tenant_id = ? AND end_user_id = ? AND id = ?`, plain, now, tenantID, endUserID, keyID); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	if _, backfillErr := usage.BackfillLegacyRequestLogsAPIKeyIDForTenant(context.Background(), tenantID, keyID, oldSecret); backfillErr != nil {
		log.WithError(backfillErr).Warnf("enduser: legacy request log backfill failed after rotating key id %s in tenant %s", keyID, tenantID)
	}
	result.PlaintextKey = plain
	result.APIKey = APIKey{ID: keyID, TenantID: tenantID, EndUserID: endUserID, KeyMasked: MaskAPIKey(plain), UpdatedAt: now}
	return result, nil
}

func (s *Service) DeleteKey(ctx context.Context, tenantID, endUserID, keyID string) error {
	if err := requireUUID(tenantID); err != nil {
		return err
	}
	if err := requireUUID(endUserID); err != nil {
		return err
	}
	if err := requireUUID(keyID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Lock end user row so concurrent deletes cannot drop below one key.
	// SQLite may not support FOR UPDATE; fall back without the lock clause.
	var lockID string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM end_users WHERE id = ? AND tenant_id = ? FOR UPDATE`, endUserID, tenantID).Scan(&lockID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err2 := tx.QueryRowContext(ctx, `SELECT id FROM end_users WHERE id = ? AND tenant_id = ?`, endUserID, tenantID).Scan(&lockID); err2 != nil {
			if errors.Is(err2, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err2
		}
	}
	var isDefault bool
	var disabledInt int
	var currentKey string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(is_default, false), disabled, key
		FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND id = ?
	`, tenantID, endUserID, keyID).Scan(&isDefault, &disabledInt, &currentKey)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	// Soft-delete keeps the row (stable id → request_logs ownership) but
	// permanently invalidates the secret so the deleted key can never auth
	// again, including via accidental re-enable in management UI.
	needTombstone := !strings.HasPrefix(strings.TrimSpace(currentKey), "sk-deleted-")
	if disabledInt != 0 {
		// Already soft-deleted/disabled: only ensure secret is tombstoned.
		if needTombstone {
			tombstone, genErr := GenerateAPIKey()
			if genErr != nil {
				return genErr
			}
			tombstone = "sk-deleted-" + strings.TrimPrefix(tombstone, "sk-")
			if _, err = tx.ExecContext(ctx, `
				UPDATE api_keys SET key = ?, is_default = false, updated_at = ?
				WHERE tenant_id = ? AND end_user_id = ? AND id = ?
			`, tombstone, now, tenantID, endUserID, keyID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0`, tenantID, endUserID).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return ErrLastKey
	}
	tombstone, genErr := GenerateAPIKey()
	if genErr != nil {
		return genErr
	}
	// Prefix makes tombstones obvious in DB; still unique via random suffix.
	tombstone = "sk-deleted-" + strings.TrimPrefix(tombstone, "sk-")
	if _, err = tx.ExecContext(ctx, `
		UPDATE api_keys SET key = ?, disabled = 1, is_default = false, updated_at = ?
		WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND disabled = 0
	`, tombstone, now, tenantID, endUserID, keyID); err != nil {
		return err
	}
	if isDefault {
		// promote oldest remaining
		if _, err = tx.ExecContext(ctx, `
			UPDATE api_keys SET is_default = true, updated_at = ?
			WHERE id = (
				SELECT id FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0 ORDER BY created_at ASC LIMIT 1
			)
		`, now, tenantID, endUserID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
