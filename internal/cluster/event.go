package cluster

import "encoding/json"

// Topics published on the cluster bus. A topic names what changed; the
// payload names which row and version, so a receiver can re-read it.
const (
	// TopicAuth announces a credential write: AuthEvent.
	TopicAuth = "auth"
	// TopicConfig announces a management-side configuration write: ConfigEvent.
	TopicConfig = "config"
	// TopicCooldown announces that an upstream account or model entered a
	// cooldown on the publishing node: CooldownEvent.
	TopicCooldown = "cooldown"
	// TopicOAuth announces a delivered OAuth callback or session change.
	TopicOAuth = "oauth"
)

// maxPayloadBytes keeps a published event under PostgreSQL's 8000-byte
// NOTIFY payload limit, leaving room for the envelope.
const maxPayloadBytes = 7000

// Event is one cross-node notification as seen by a subscriber.
type Event struct {
	// Topic is one of the Topic* constants.
	Topic string `json:"topic"`
	// Origin is the node ID of the publisher.
	Origin string `json:"origin"`
	// Payload is the topic-specific JSON document.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Resync marks the synthetic event delivered after the bus listener
	// (re)connects. Events published while disconnected are lost, so a
	// subscriber that caches state must reload all of it.
	Resync bool `json:"-"`
}

// Decode unmarshals the payload into v.
func (e Event) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// AuthEvent is the payload of TopicAuth.
type AuthEvent struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Deleted bool   `json:"deleted,omitempty"`
}

// ConfigEvent is the payload of TopicConfig. Domain names the reload unit
// (for example "api_keys", "routing", "proxy_pool", "runtime_settings",
// "model_configs", "pricing", "ip_access_policy", "tenants"); Key and
// Version are optional refinements for domains stored per key.
type ConfigEvent struct {
	Domain   string `json:"domain"`
	TenantID string `json:"tenant_id,omitempty"`
	Key      string `json:"key,omitempty"`
	Version  int64  `json:"version,omitempty"`
}

// CooldownEvent is the payload of TopicCooldown. Receivers only ever extend
// an existing cooldown, never shorten it, so reordering is harmless.
type CooldownEvent struct {
	AuthID     string `json:"auth_id"`
	Model      string `json:"model,omitempty"`
	UntilUnix  int64  `json:"until_unix"`
	StatusCode int    `json:"status_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// OAuthEvent is the payload of TopicOAuth: the shared session State received
// its provider callback or stopped being pending. Only the node that started
// the login waits on it; that node re-reads the session row.
type OAuthEvent struct {
	State string `json:"state"`
}
