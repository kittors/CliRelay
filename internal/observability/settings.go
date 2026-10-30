package observability

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
)

// SettingKey is the runtime_settings row (system tenant) holding Settings.
const SettingKey = "observability-metrics"

// MetricsPath is where the exposition is served.
const MetricsPath = "/metrics"

// tokenPrefix makes a leaked scrape token recognisable to secret scanners and
// to whoever finds it in a Prometheus config.
const tokenPrefix = "crm_"

func init() {
	// The setting's receiver is this package, not a config field, so its
	// writes are announced under their own domain and other nodes (and the
	// other blue/green slot) reload it from the store.
	configsync.RouteRuntimeSettingKey(SettingKey, configsync.DomainObservability)
}

// ErrNotPersistent is returned by writes when no store is attached: a setting
// that took effect but would be gone after the next restart is worse than one
// that was refused.
var ErrNotPersistent = errors.New("observability settings cannot be saved: no settings store is attached")

// Settings is the stored form. Only a SHA-256 of the scrape token is kept; the
// token itself is shown once, when it is generated.
type Settings struct {
	Enabled        bool      `json:"enabled"`
	TokenSHA256    string    `json:"token_sha256,omitempty"`
	TokenHint      string    `json:"token_hint,omitempty"`
	TokenCreatedAt time.Time `json:"token_created_at,omitzero"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
}

// HasToken reports whether a scrape token is configured.
func (s Settings) HasToken() bool { return strings.TrimSpace(s.TokenSHA256) != "" }

// Persister loads and saves Settings. It is an interface so this package,
// which sits on the request path, never depends on the settings store.
type Persister interface {
	Load() (Settings, bool, error)
	Save(Settings) error
}

type settingsState struct {
	mu        sync.Mutex
	current   atomic.Pointer[Settings]
	persister Persister
}

// Settings returns the settings in force.
func (e *Exporter) Settings() Settings {
	if e == nil {
		return Settings{}
	}
	if s := e.settings.current.Load(); s != nil {
		return *s
	}
	return Settings{}
}

// Persistent reports whether writes are saved.
func (e *Exporter) Persistent() bool {
	if e == nil {
		return false
	}
	e.settings.mu.Lock()
	defer e.settings.mu.Unlock()
	return e.settings.persister != nil
}

// SetPersister attaches the store and adopts what it holds.
func (e *Exporter) SetPersister(persister Persister) error {
	if e == nil || persister == nil {
		return nil
	}
	e.settings.mu.Lock()
	e.settings.persister = persister
	e.settings.mu.Unlock()
	return e.Reload()
}

// Reload re-reads the stored settings, for a change saved by another node or
// by the other blue/green slot. A missing row means the defaults: disabled.
func (e *Exporter) Reload() error {
	if e == nil {
		return nil
	}
	e.settings.mu.Lock()
	defer e.settings.mu.Unlock()
	if e.settings.persister == nil {
		return nil
	}
	stored, ok, err := e.settings.persister.Load()
	if err != nil {
		return err
	}
	if !ok {
		stored = Settings{}
	}
	e.settings.current.Store(&stored)
	return nil
}

// SetEnabled turns the endpoint on or off. Turning it on without a token
// generates one and returns it; it is the only time it can be read. Turning it
// off keeps the token, so a scraper configured with it works again once the
// endpoint is back.
func (e *Exporter) SetEnabled(enabled bool) (Settings, string, error) {
	return e.update(func(s *Settings) (string, error) {
		s.Enabled = enabled
		if enabled && !s.HasToken() {
			return issueToken(s, e.now())
		}
		return "", nil
	})
}

// RotateToken replaces the scrape token and returns the new one. The old one
// stops working at once.
func (e *Exporter) RotateToken() (Settings, string, error) {
	return e.update(func(s *Settings) (string, error) {
		return issueToken(s, e.now())
	})
}

// update saves first and applies second, like the IP access policy: a change
// that took effect but did not survive a restart would silently revert.
func (e *Exporter) update(mutate func(*Settings) (string, error)) (Settings, string, error) {
	if e == nil {
		return Settings{}, "", ErrNotPersistent
	}
	e.settings.mu.Lock()
	defer e.settings.mu.Unlock()
	if e.settings.persister == nil {
		return Settings{}, "", ErrNotPersistent
	}
	next := Settings{}
	if current := e.settings.current.Load(); current != nil {
		next = *current
	}
	token, err := mutate(&next)
	if err != nil {
		return Settings{}, "", err
	}
	next.UpdatedAt = e.now().UTC()
	if err := e.settings.persister.Save(next); err != nil {
		return Settings{}, "", err
	}
	e.settings.current.Store(&next)
	return next, token, nil
}

func issueToken(s *Settings, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	s.TokenSHA256 = hashToken(token)
	s.TokenHint = token[len(token)-4:]
	s.TokenCreatedAt = now.UTC()
	return token, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenMatches compares in constant time. The hash is compared rather than
// the token, so the comparison's length never depends on what was presented.
func tokenMatches(presented, wantSHA256 string) bool {
	presented = strings.TrimSpace(presented)
	if presented == "" || wantSHA256 == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashToken(presented)), []byte(wantSHA256)) == 1
}
