package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

func table(name string) []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: name}}
}

type RequestLog struct{ ent.Schema }

func (RequestLog) Annotations() []schema.Annotation { return table("request_logs") }
func (RequestLog) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Time("timestamp").Default(time.Now),
		field.String("api_key").Default(""),
		field.String("api_key_id").Default(""),
		field.String("auth_subject_id").Default(""),
		field.String("api_key_name").Default(""),
		field.String("model").Default(""),
		field.String("upstream_model").Default(""),
		field.String("vision_fallback_model").Default(""),
		field.String("source").Default(""),
		field.String("channel_name").Default(""),
		field.String("auth_index").Default(""),
		field.Int("failed").Default(0),
		field.Int("streaming").Default(0),
		field.Int64("latency_ms").Default(0),
		field.Int64("first_token_ms").Default(0),
		field.Int64("input_tokens").Default(0),
		field.Int64("output_tokens").Default(0),
		field.Int64("reasoning_tokens").Default(0),
		field.Int64("cached_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Float("cost").Default(0),
		field.String("input_content").Default(""),
		field.String("output_content").Default(""),
	}
}
func (RequestLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("timestamp"),
		index.Fields("api_key"),
		index.Fields("api_key", "timestamp"),
		index.Fields("api_key_id"),
		index.Fields("api_key_id", "timestamp"),
		index.Fields("model"),
		index.Fields("failed"),
		index.Fields("auth_index"),
		index.Fields("auth_subject_id"),
	}
}

type RequestLogContent struct{ ent.Schema }

func (RequestLogContent) Annotations() []schema.Annotation { return table("request_log_content") }
func (RequestLogContent) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").StorageKey("log_id"),
		field.Time("timestamp").Default(time.Now),
		field.String("compression").Default("zstd"),
		field.Bytes("input_content").Optional(),
		field.Bytes("output_content").Optional(),
		field.Bytes("detail_content").Optional(),
		field.String("session_id").Default(""),
	}
}
func (RequestLogContent) Indexes() []ent.Index {
	return []ent.Index{index.Fields("timestamp"), index.Fields("session_id", "timestamp")}
}

type AuthFileQuotaSnapshot struct{ ent.Schema }

func (AuthFileQuotaSnapshot) Annotations() []schema.Annotation {
	return table("auth_file_quota_snapshots")
}
func (AuthFileQuotaSnapshot) Fields() []ent.Field {
	return []ent.Field{
		field.String("date_key"),
		field.String("auth_index"),
		field.String("auth_subject_id").Default(""),
		field.String("provider").Default(""),
		field.String("quota_key"),
		field.Float("percent").Optional().Nillable(),
		field.Time("recorded_at"),
	}
}
func (AuthFileQuotaSnapshot) Indexes() []ent.Index {
	return []ent.Index{index.Fields("date_key", "auth_index", "quota_key").Unique(), index.Fields("auth_index"), index.Fields("auth_subject_id")}
}

type AuthFileQuotaSnapshotPoint struct{ ent.Schema }

func (AuthFileQuotaSnapshotPoint) Annotations() []schema.Annotation {
	return table("auth_file_quota_snapshot_points")
}
func (AuthFileQuotaSnapshotPoint) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Time("recorded_at"),
		field.String("auth_index"),
		field.String("auth_subject_id").Default(""),
		field.String("provider").Default(""),
		field.String("quota_key"),
		field.String("quota_label").Default(""),
		field.Float("percent").Optional().Nillable(),
		field.Time("reset_at").Optional().Nillable(),
		field.Int64("window_seconds").Default(0),
	}
}
func (AuthFileQuotaSnapshotPoint) Indexes() []ent.Index {
	return []ent.Index{index.Fields("auth_index", "recorded_at"), index.Fields("auth_index", "quota_key", "recorded_at"), index.Fields("auth_subject_id", "recorded_at")}
}

type AuthSubjectQuotaCycle struct{ ent.Schema }

func (AuthSubjectQuotaCycle) Annotations() []schema.Annotation {
	return table("auth_subject_quota_cycles")
}
func (AuthSubjectQuotaCycle) Fields() []ent.Field {
	return []ent.Field{
		field.String("subject_id"),
		field.String("auth_index").Default(""),
		field.String("provider").Default(""),
		field.String("quota_key"),
		field.Time("cycle_start_at"),
		field.Time("reset_at"),
		field.Int64("window_seconds").Default(0),
		field.Time("last_verified_at"),
	}
}
func (AuthSubjectQuotaCycle) Indexes() []ent.Index {
	return []ent.Index{index.Fields("subject_id", "quota_key").Unique(), index.Fields("subject_id", "window_seconds", "last_verified_at")}
}

type ModelPricing struct{ ent.Schema }

func (ModelPricing) Annotations() []schema.Annotation { return table("model_pricing") }
func (ModelPricing) Fields() []ent.Field {
	return []ent.Field{
		field.String("model_id").Unique(),
		field.Float("input_price_per_million").Default(0),
		field.Float("output_price_per_million").Default(0),
		field.Float("cached_price_per_million").Default(0),
		field.Float("cache_read_price_per_million").Default(0),
		field.Float("cache_write_price_per_million").Default(0),
		field.Time("updated_at"),
	}
}

type APIKeyPermissionProfile struct{ ent.Schema }

func (APIKeyPermissionProfile) Annotations() []schema.Annotation {
	return table("api_key_permission_profiles")
}
func (APIKeyPermissionProfile) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique(),
		field.String("name").Default(""),
		field.Int("daily_limit").Default(0),
		field.Int("total_quota").Default(0),
		field.Int("concurrency_limit").Default(0),
		field.Int("rpm_limit").Default(0),
		field.Int("tpm_limit").Default(0),
		field.String("allowed_models").Default("[]"),
		field.String("allowed_channels").Default("[]"),
		field.String("allowed_channel_groups").Default("[]"),
		field.String("system_prompt").Default(""),
		field.String("created_at").Default(""),
		field.String("updated_at").Default(""),
	}
}

type APIKey struct{ ent.Schema }

func (APIKey) Annotations() []schema.Annotation { return table("api_keys") }
func (APIKey) Fields() []ent.Field {
	return []ent.Field{
		field.String("key").Unique(),
		field.String("id").Unique(),
		field.String("name").Default(""),
		field.Int("disabled").Default(0),
		field.String("permission_profile_id").Default(""),
		field.Int("daily_limit").Default(0),
		field.Int("total_quota").Default(0),
		field.Float("spending_limit").Default(0),
		field.Float("daily_spending_limit").Default(0),
		field.Int("concurrency_limit").Default(0),
		field.Int("rpm_limit").Default(0),
		field.Int("tpm_limit").Default(0),
		field.String("allowed_models").Default("[]"),
		field.String("allowed_channels").Default("[]"),
		field.String("allowed_channel_groups").Default("[]"),
		field.String("system_prompt").Default(""),
		field.String("created_at").Default(""),
		field.String("updated_at").Default(""),
	}
}

type ModelConfig struct{ ent.Schema }

func (ModelConfig) Annotations() []schema.Annotation { return table("model_configs") }
func (ModelConfig) Fields() []ent.Field {
	return []ent.Field{
		field.String("model_id").Unique(),
		field.String("owned_by").Default(""),
		field.String("description").Default(""),
		field.Int("enabled").Default(1),
		field.String("input_modalities").Default(""),
		field.String("output_modalities").Default(""),
		field.String("pricing_mode").Default("token"),
		field.Float("input_price_per_million").Default(0),
		field.Float("output_price_per_million").Default(0),
		field.Float("cached_price_per_million").Default(0),
		field.Float("cache_read_price_per_million").Default(0),
		field.Float("cache_write_price_per_million").Default(0),
		field.Float("price_per_call").Default(0),
		field.String("source").Default("user"),
		field.Time("updated_at"),
	}
}
func (ModelConfig) Indexes() []ent.Index { return []ent.Index{index.Fields("owned_by")} }

type ModelOwnerPreset struct{ ent.Schema }

func (ModelOwnerPreset) Annotations() []schema.Annotation { return table("model_owner_presets") }
func (ModelOwnerPreset) Fields() []ent.Field {
	return []ent.Field{
		field.String("value").Unique(),
		field.String("label").Default(""),
		field.String("description").Default(""),
		field.Int("enabled").Default(1),
		field.Time("updated_at"),
	}
}

type AuthGroupModelOwnerMapping struct{ ent.Schema }

func (AuthGroupModelOwnerMapping) Annotations() []schema.Annotation {
	return table("auth_group_model_owner_mappings")
}
func (AuthGroupModelOwnerMapping) Fields() []ent.Field {
	return []ent.Field{
		field.String("auth_group").Unique(),
		field.String("owner").Default(""),
		field.Time("updated_at"),
	}
}
func (AuthGroupModelOwnerMapping) Indexes() []ent.Index { return []ent.Index{index.Fields("owner")} }

type ModelOpenrouterSyncState struct{ ent.Schema }

func (ModelOpenrouterSyncState) Annotations() []schema.Annotation {
	return table("model_openrouter_sync_state")
}
func (ModelOpenrouterSyncState) Fields() []ent.Field {
	return []ent.Field{
		field.Int("id").Default(1),
		field.Int("enabled").Default(0),
		field.Int("interval_minutes").Default(1440),
		field.String("last_sync_at").Default(""),
		field.String("last_success_at").Default(""),
		field.String("last_error").Default(""),
		field.Int("last_seen").Default(0),
		field.Int("last_added").Default(0),
		field.Int("last_updated").Default(0),
		field.Int("last_skipped").Default(0),
		field.Time("updated_at"),
	}
}

type ProxyPool struct{ ent.Schema }

func (ProxyPool) Annotations() []schema.Annotation { return table("proxy_pool") }
func (ProxyPool) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique(),
		field.String("name").Default(""),
		field.String("url"),
		field.Int("enabled").Default(1),
		field.String("description").Default(""),
		field.String("created_at").Default(""),
		field.String("updated_at").Default(""),
	}
}

type RoutingConfig struct{ ent.Schema }

func (RoutingConfig) Annotations() []schema.Annotation { return table("routing_config") }
func (RoutingConfig) Fields() []ent.Field {
	return []ent.Field{
		field.Int("id").Default(1),
		field.String("payload").Default("{}"),
		field.String("updated_at").Default(""),
	}
}

type RuntimeSetting struct{ ent.Schema }

func (RuntimeSetting) Annotations() []schema.Annotation { return table("runtime_settings") }
func (RuntimeSetting) Fields() []ent.Field {
	return []ent.Field{
		field.String("setting_key").Unique(),
		field.String("payload").Default("{}"),
		field.String("updated_at").Default(""),
	}
}

type IdentityFingerprint struct{ ent.Schema }

func (IdentityFingerprint) Annotations() []schema.Annotation { return table("identity_fingerprints") }
func (IdentityFingerprint) Fields() []ent.Field {
	return []ent.Field{
		field.String("provider"),
		field.String("account_key"),
		field.String("auth_subject_id").Default(""),
		field.String("client_product").Default(""),
		field.String("client_variant").Default(""),
		field.String("version").Default(""),
		field.String("fields_json").Default("{}"),
		field.String("observed_headers_json").Default("{}"),
		field.String("created_at").Default(""),
		field.String("updated_at").Default(""),
		field.String("last_seen_at").Default(""),
	}
}
func (IdentityFingerprint) Indexes() []ent.Index {
	return []ent.Index{index.Fields("provider", "account_key").Unique(), index.Fields("provider", "last_seen_at")}
}

type CcSwitchImportConfig struct{ ent.Schema }

func (CcSwitchImportConfig) Annotations() []schema.Annotation {
	return table("ccswitch_import_configs")
}
func (CcSwitchImportConfig) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique(),
		field.String("client_type"),
		field.String("provider_name").Default(""),
		field.String("note").Default(""),
		field.String("default_model").Default(""),
		field.String("model_mappings").Default("[]"),
		field.String("allowed_channel_groups").Default("[]"),
		field.String("route_path").Default(""),
		field.String("endpoint_path").Default(""),
		field.Int("usage_auto_interval").Default(30),
		field.String("api_key_field").Default(""),
		field.String("created_at").Default(""),
		field.String("updated_at").Default(""),
	}
}
func (CcSwitchImportConfig) Indexes() []ent.Index {
	return []ent.Index{index.Fields("route_path").Unique()}
}
