package billing

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// sqliteTablesSQL creates the billing tables on SQLite test databases using the
// exact same columns and constraints as internal/storage/postgres/billing_migrations.go.
const sqliteTablesSQL = `
CREATE TABLE IF NOT EXISTS billing_settings (
  tenant_id               TEXT PRIMARY KEY,
  enabled                 INTEGER NOT NULL DEFAULT 0,
  provider                TEXT NOT NULL DEFAULT 'xunhupay',
  public_base_url         TEXT NOT NULL DEFAULT '',
  xunhu_gateway_url       TEXT NOT NULL DEFAULT 'https://api.xunhupay.com/payment/do.html',
  xunhu_query_url         TEXT NOT NULL DEFAULT 'https://api.xunhupay.com/payment/query.html',
  xunhu_wechat_enabled    INTEGER NOT NULL DEFAULT 0,
  xunhu_wechat_appid      TEXT NOT NULL DEFAULT '',
  xunhu_wechat_appsecret  TEXT NOT NULL DEFAULT '',
  xunhu_alipay_enabled    INTEGER NOT NULL DEFAULT 0,
  xunhu_alipay_appid      TEXT NOT NULL DEFAULT '',
  xunhu_alipay_appsecret  TEXT NOT NULL DEFAULT '',
  order_expire_minutes    INTEGER NOT NULL DEFAULT 15,
  currency                TEXT NOT NULL DEFAULT 'CNY',
  updated_at              TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_by              TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS billing_plans (
  id                    TEXT PRIMARY KEY,
  tenant_id             TEXT NOT NULL,
  name                  TEXT NOT NULL,
  description           TEXT NOT NULL DEFAULT '',
  kind                  TEXT NOT NULL CHECK (kind IN ('subscription', 'quota_pack', 'custom_quota', 'daily_reset')),
  price_cents           INTEGER NOT NULL DEFAULT 0,
  duration_days         INTEGER NOT NULL DEFAULT 0,
  quota_amount_usd      REAL NOT NULL DEFAULT 0,
  quota_requests        INTEGER NOT NULL DEFAULT 0,
  quota_tokens          INTEGER NOT NULL DEFAULT 0,
  rate_usd_per_cny      REAL NOT NULL DEFAULT 0,
  min_amount_cents      INTEGER NOT NULL DEFAULT 100,
  max_amount_cents      INTEGER NOT NULL DEFAULT 100000,
  permission_profile_id TEXT NOT NULL DEFAULT '',
  badge                 TEXT NOT NULL DEFAULT '',
  sort_order            INTEGER NOT NULL DEFAULT 0,
  enabled               INTEGER NOT NULL DEFAULT 1,
  created_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS billing_bindings (
  id                   TEXT PRIMARY KEY,
  tenant_id            TEXT NOT NULL,
  target_kind          TEXT NOT NULL CHECK (target_kind IN ('end_user', 'api_key')),
  target_id            TEXT NOT NULL,
  end_user_id          TEXT NOT NULL DEFAULT '',
  enabled              INTEGER NOT NULL DEFAULT 1,
  plan_ids_json        TEXT NOT NULL DEFAULT '[]',
  price_overrides_json TEXT NOT NULL DEFAULT '{}',
  public_code          TEXT NOT NULL DEFAULT '',
  allow_public_pay     INTEGER NOT NULL DEFAULT 0,
  note                 TEXT NOT NULL DEFAULT '',
  created_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_bindings_target
  ON billing_bindings(tenant_id, target_kind, target_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_bindings_public_code
  ON billing_bindings(public_code) WHERE public_code <> '';

CREATE TABLE IF NOT EXISTS billing_entitlements (
  tenant_id   TEXT NOT NULL,
  target_kind TEXT NOT NULL CHECK (target_kind IN ('end_user', 'api_key')),
  target_id   TEXT NOT NULL,
  expires_at  TIMESTAMP,
  updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (tenant_id, target_kind, target_id)
);

CREATE TABLE IF NOT EXISTS billing_orders (
  id                  TEXT PRIMARY KEY,
  order_no            TEXT NOT NULL UNIQUE,
  tenant_id           TEXT NOT NULL,
  binding_id          TEXT NOT NULL DEFAULT '',
  target_kind         TEXT NOT NULL CHECK (target_kind IN ('end_user', 'api_key')),
  target_id           TEXT NOT NULL,
  target_label        TEXT NOT NULL DEFAULT '',
  end_user_id         TEXT NOT NULL DEFAULT '',
  plan_id             TEXT NOT NULL DEFAULT '',
  plan_name           TEXT NOT NULL DEFAULT '',
  plan_kind           TEXT NOT NULL DEFAULT '',
  plan_snapshot_json  TEXT NOT NULL DEFAULT '{}',
  provider            TEXT NOT NULL DEFAULT 'xunhupay',
  pay_channel         TEXT NOT NULL DEFAULT 'wechat',
  amount_cents        INTEGER NOT NULL,
  currency            TEXT NOT NULL DEFAULT 'CNY',
  status              TEXT NOT NULL DEFAULT 'pending',
  provider_order_id   TEXT NOT NULL DEFAULT '',
  provider_trade_no   TEXT NOT NULL DEFAULT '',
  qrcode_url          TEXT NOT NULL DEFAULT '',
  pay_url             TEXT NOT NULL DEFAULT '',
  fulfillment_summary TEXT NOT NULL DEFAULT '',
  fulfillment_error   TEXT NOT NULL DEFAULT '',
  source              TEXT NOT NULL DEFAULT 'portal',
  created_by          TEXT NOT NULL DEFAULT '',
  expires_at          TIMESTAMP NOT NULL,
  paid_at             TIMESTAMP,
  fulfilled_at        TIMESTAMP,
  created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// InitSQLiteTables initializes billing tables on a SQLite database (tests / local dev).
func InitSQLiteTables(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(sqliteTablesSQL); err != nil {
		return fmt.Errorf("billing: init sqlite tables: %w", err)
	}
	return nil
}

// parseAnyTime handles both native time.Time (Postgres / SQLite TIMESTAMP) and
// RFC3339 / SQL timestamp strings returned by SQLite drivers.
func parseAnyTime(raw any) (time.Time, bool) {
	switch v := raw.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		if v.IsZero() {
			return time.Time{}, false
		}
		return v.UTC(), true
	case []byte:
		return parseTimeString(string(v))
	case string:
		return parseTimeString(v)
	default:
		return time.Time{}, false
	}
}

func parseTimeString(raw string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
