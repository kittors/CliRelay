package postgres

// billingTablesSQL adds tenant-scoped self-service payment settings (XunhuPay),
// reusable billing plans (subscription, quota pack, custom quota, daily reset),
// per-account / per-key payment bindings with optional shareable pay codes,
// subscription expiration entitlements, and the payment order ledger.
const billingTablesSQL = `
CREATE TABLE IF NOT EXISTS billing_settings (
  tenant_id               UUID PRIMARY KEY,
  enabled                 BOOLEAN NOT NULL DEFAULT false,
  provider                TEXT NOT NULL DEFAULT 'xunhupay',
  public_base_url         TEXT NOT NULL DEFAULT '',
  xunhu_gateway_url       TEXT NOT NULL DEFAULT 'https://api.xunhupay.com/payment/do.html',
  xunhu_query_url         TEXT NOT NULL DEFAULT 'https://api.xunhupay.com/payment/query.html',
  xunhu_wechat_enabled    BOOLEAN NOT NULL DEFAULT false,
  xunhu_wechat_appid      TEXT NOT NULL DEFAULT '',
  xunhu_wechat_appsecret  TEXT NOT NULL DEFAULT '',
  xunhu_alipay_enabled    BOOLEAN NOT NULL DEFAULT false,
  xunhu_alipay_appid      TEXT NOT NULL DEFAULT '',
  xunhu_alipay_appsecret  TEXT NOT NULL DEFAULT '',
  order_expire_minutes    INTEGER NOT NULL DEFAULT 15 CHECK (order_expire_minutes >= 1 AND order_expire_minutes <= 1440),
  currency                TEXT NOT NULL DEFAULT 'CNY',
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by              TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS billing_plans (
  id                    TEXT PRIMARY KEY,
  tenant_id             UUID NOT NULL,
  name                  TEXT NOT NULL,
  description           TEXT NOT NULL DEFAULT '',
  kind                  TEXT NOT NULL CHECK (kind IN ('subscription', 'quota_pack', 'custom_quota', 'daily_reset')),
  price_cents           INTEGER NOT NULL DEFAULT 0 CHECK (price_cents >= 0),
  duration_days         INTEGER NOT NULL DEFAULT 0 CHECK (duration_days >= 0),
  quota_amount_usd      DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (quota_amount_usd >= 0),
  quota_requests        BIGINT NOT NULL DEFAULT 0 CHECK (quota_requests >= 0),
  quota_tokens          BIGINT NOT NULL DEFAULT 0 CHECK (quota_tokens >= 0),
  rate_usd_per_cny      DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (rate_usd_per_cny >= 0),
  min_amount_cents      INTEGER NOT NULL DEFAULT 100 CHECK (min_amount_cents >= 0),
  max_amount_cents      INTEGER NOT NULL DEFAULT 100000 CHECK (max_amount_cents >= 0),
  permission_profile_id TEXT NOT NULL DEFAULT '',
  badge                 TEXT NOT NULL DEFAULT '',
  sort_order            INTEGER NOT NULL DEFAULT 0,
  enabled               BOOLEAN NOT NULL DEFAULT true,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_billing_plans_tenant_sort
  ON billing_plans(tenant_id, enabled, sort_order, created_at);

CREATE TABLE IF NOT EXISTS billing_bindings (
  id                   TEXT PRIMARY KEY,
  tenant_id            UUID NOT NULL,
  target_kind          TEXT NOT NULL CHECK (target_kind IN ('end_user', 'api_key')),
  target_id            TEXT NOT NULL,
  end_user_id          TEXT NOT NULL DEFAULT '',
  enabled              BOOLEAN NOT NULL DEFAULT true,
  plan_ids_json        TEXT NOT NULL DEFAULT '[]',
  price_overrides_json TEXT NOT NULL DEFAULT '{}',
  public_code          TEXT NOT NULL DEFAULT '',
  allow_public_pay     BOOLEAN NOT NULL DEFAULT false,
  note                 TEXT NOT NULL DEFAULT '',
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_bindings_target
  ON billing_bindings(tenant_id, target_kind, target_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_bindings_public_code
  ON billing_bindings(public_code) WHERE public_code <> '';
CREATE INDEX IF NOT EXISTS idx_billing_bindings_tenant_end_user
  ON billing_bindings(tenant_id, end_user_id);

CREATE TABLE IF NOT EXISTS billing_entitlements (
  tenant_id   UUID NOT NULL,
  target_kind TEXT NOT NULL CHECK (target_kind IN ('end_user', 'api_key')),
  target_id   TEXT NOT NULL,
  expires_at  TIMESTAMPTZ,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, target_kind, target_id)
);
CREATE INDEX IF NOT EXISTS idx_billing_entitlements_expires_at
  ON billing_entitlements(expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS billing_orders (
  id                  TEXT PRIMARY KEY,
  order_no            TEXT NOT NULL UNIQUE,
  tenant_id           UUID NOT NULL,
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
  amount_cents        INTEGER NOT NULL CHECK (amount_cents >= 0),
  currency            TEXT NOT NULL DEFAULT 'CNY',
  status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid', 'fulfilled', 'failed', 'expired', 'closed')),
  provider_order_id   TEXT NOT NULL DEFAULT '',
  provider_trade_no   TEXT NOT NULL DEFAULT '',
  qrcode_url          TEXT NOT NULL DEFAULT '',
  pay_url             TEXT NOT NULL DEFAULT '',
  fulfillment_summary TEXT NOT NULL DEFAULT '',
  fulfillment_error   TEXT NOT NULL DEFAULT '',
  source              TEXT NOT NULL DEFAULT 'portal',
  created_by          TEXT NOT NULL DEFAULT '',
  expires_at          TIMESTAMPTZ NOT NULL,
  paid_at             TIMESTAMPTZ,
  fulfilled_at        TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_billing_orders_tenant_created
  ON billing_orders(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_orders_tenant_user_created
  ON billing_orders(tenant_id, end_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_orders_tenant_status
  ON billing_orders(tenant_id, status, created_at DESC);
`
