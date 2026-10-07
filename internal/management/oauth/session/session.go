package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTTL      = 10 * time.Minute
	MaxStateLength  = 128
	StatusCompleted = "__completed__"
	// StatusSuperseded marks a pending login that another completed login of the
	// same provider replaced (see CompleteProviderTenant). It is kept as a
	// tombstone rather than deleted so a status poll can tell "replaced by a
	// finished login" (reported as done, as before) apart from "expired or never
	// seen" (reported as expired) — the two used to be indistinguishable.
	StatusSuperseded = "__superseded__"
)

var (
	ErrInvalidState    = errors.New("invalid oauth state")
	ErrUnsupportedFlow = errors.New("unsupported oauth provider")
	ErrNotPending      = errors.New("oauth session is not pending")
	ErrCallbackTimeout = errors.New("timeout waiting for oauth callback")
)

type Session struct {
	Provider  string
	TenantID  string
	Status    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type Store struct {
	mu       sync.RWMutex
	ttl      time.Duration
	sessions map[string]Session
}

func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Store{
		ttl:      ttl,
		sessions: make(map[string]Session),
	}
}

func (s *Store) purgeExpiredLocked(now time.Time) {
	for state, session := range s.sessions {
		if !session.ExpiresAt.IsZero() && now.After(session.ExpiresAt) {
			delete(s.sessions, state)
		}
	}
}

func (s *Store) Register(state, provider string) {
	s.RegisterTenant(state, provider, "")
}

func (s *Store) RegisterTenant(state, provider, tenantID string) {
	if s == nil {
		return
	}
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if state == "" || provider == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	s.sessions[state] = Session{
		Provider:  provider,
		TenantID:  strings.TrimSpace(tenantID),
		Status:    "",
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
}

func (s *Store) SetError(state, message string) {
	if s == nil {
		return
	}
	state = strings.TrimSpace(state)
	message = strings.TrimSpace(message)
	if state == "" {
		return
	}
	if message == "" {
		message = "Authentication failed"
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		return
	}
	session.Status = message
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
}

func (s *Store) Complete(state string) {
	if s == nil {
		return
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		return
	}
	session.Status = StatusCompleted
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
}

func (s *Store) CompleteProvider(provider string) int {
	return s.CompleteProviderTenant(provider, "")
}

func (s *Store) CompleteProviderTenant(provider, tenantID string) int {
	if s == nil {
		return 0
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	tenantID = strings.TrimSpace(tenantID)
	if provider == "" {
		return 0
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	superseded := 0
	for state, session := range s.sessions {
		if strings.EqualFold(session.Provider, provider) && (tenantID == "" || session.TenantID == tenantID) {
			if session.Status == StatusCompleted || session.Status == StatusSuperseded {
				continue
			}
			session.Status = StatusSuperseded
			session.ExpiresAt = now.Add(s.ttl)
			s.sessions[state] = session
			superseded++
		}
	}
	return superseded
}

func (s *Store) Get(state string) (Session, bool) {
	if s == nil {
		return Session{}, false
	}
	state = strings.TrimSpace(state)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	return session, ok
}

func (s *Store) IsPending(state, provider string) bool {
	if s == nil {
		return false
	}
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok {
		return false
	}
	if session.Status != "" {
		return false
	}
	if provider == "" {
		return true
	}
	return strings.EqualFold(session.Provider, provider)
}

func ValidateState(state string) error {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return fmt.Errorf("%w: empty", ErrInvalidState)
	}
	if len(trimmed) > MaxStateLength {
		return fmt.Errorf("%w: too long", ErrInvalidState)
	}
	if strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\") {
		return fmt.Errorf("%w: contains path separator", ErrInvalidState)
	}
	if strings.Contains(trimmed, "..") {
		return fmt.Errorf("%w: contains '..'", ErrInvalidState)
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("%w: invalid character", ErrInvalidState)
		}
	}
	return nil
}

func NormalizeProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "anthropic", nil
	case "codex", "openai":
		return "codex", nil
	case "gemini", "google":
		return "gemini", nil
	case "iflow", "i-flow":
		return "iflow", nil
	case "antigravity", "anti-gravity":
		return "antigravity", nil
	case "qwen":
		return "qwen", nil
	case "xai", "x-ai", "x.ai", "grok":
		return "xai", nil
	default:
		return "", ErrUnsupportedFlow
	}
}

type callbackFilePayload struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

func WriteCallbackFile(authDir, provider, state, code, errorMessage string) (string, error) {
	if strings.TrimSpace(authDir) == "" {
		return "", fmt.Errorf("auth dir is empty")
	}
	canonicalProvider, err := NormalizeProvider(provider)
	if err != nil {
		return "", err
	}
	if err := ValidateState(state); err != nil {
		return "", err
	}

	fileName := fmt.Sprintf(".oauth-%s-%s.oauth", canonicalProvider, state)
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", fmt.Errorf("prepare oauth callback directory: %w", err)
	}
	filePath := filepath.Join(authDir, fileName)
	payload := callbackFilePayload{
		Code:  strings.TrimSpace(code),
		State: strings.TrimSpace(state),
		Error: strings.TrimSpace(errorMessage),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal oauth callback payload: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return "", fmt.Errorf("write oauth callback file: %w", err)
	}
	return filePath, nil
}

func (s *Store) WriteCallbackFileForPending(authDir, provider, state, code, errorMessage string) (string, error) {
	canonicalProvider, err := NormalizeProvider(provider)
	if err != nil {
		return "", err
	}
	if !s.IsPending(state, canonicalProvider) {
		return "", ErrNotPending
	}
	return WriteCallbackFile(authDir, canonicalProvider, state, code, errorMessage)
}

func (s *Store) WaitCallbackFile(authDir, provider, state string, timeout, pollInterval time.Duration) (map[string]string, error) {
	canonicalProvider, err := NormalizeProvider(provider)
	if err != nil {
		return nil, err
	}
	if err := ValidateState(state); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultTTL
	}
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}

	fileName := fmt.Sprintf(".oauth-%s-%s.oauth", canonicalProvider, state)
	filePath := filepath.Join(authDir, fileName)
	deadline := time.Now().Add(timeout)
	for {
		if !s.IsPending(state, canonicalProvider) {
			return nil, ErrNotPending
		}
		if time.Now().After(deadline) {
			return nil, ErrCallbackTimeout
		}
		if data, errRead := os.ReadFile(filePath); errRead == nil {
			var payload map[string]string
			_ = json.Unmarshal(data, &payload)
			_ = os.Remove(filePath)
			return payload, nil
		}
		time.Sleep(pollInterval)
	}
}
