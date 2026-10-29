package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const contentPolicyViolationCode = "content_policy_violation"

// RequestModerationResult is the narrow SDK contract returned by a host-provided
// request moderator. Dependency failures must be handled fail-open by the host.
type RequestModerationResult struct {
	Blocked    bool
	Message    string
	HTTPStatus int
}

// RequestModerator evaluates the original inbound request after an auth
// candidate is selected and before any provider executor is called.
//
// auth is not always the requester's own credential: a platform-shared
// credential keeps its owner's TenantID (the system tenant, see shared_grants.go)
// while the requesting tenant travels in opts.Metadata[TenantMetadataKey].
// Implementations that scope policy by tenant must decide which of the two it
// belongs to; the manager calls them either way.
type RequestModerator interface {
	Moderate(ctx context.Context, auth *Auth, opts cliproxyexecutor.Options) RequestModerationResult
}

func (m *Manager) SetRequestModerator(moderator RequestModerator) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.requestModerator = moderator
	m.mu.Unlock()
}

func (m *Manager) moderateRequest(ctx context.Context, auth *Auth, opts cliproxyexecutor.Options) error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	moderator := m.requestModerator
	m.mu.RUnlock()
	if moderator == nil {
		return nil
	}
	result := moderator.Moderate(ctx, auth, opts)
	if !result.Blocked {
		return nil
	}
	status := result.HTTPStatus
	if status < 400 || status > 599 {
		status = http.StatusForbidden
	}
	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = "Your request was blocked by the content moderation policy."
	}
	return &Error{Code: contentPolicyViolationCode, Message: message, HTTPStatus: status}
}

func isContentPolicyViolation(err error) bool {
	var authErr *Error
	return errors.As(err, &authErr) && authErr != nil && authErr.Code == contentPolicyViolationCode
}
