package executor

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func identityFingerprintHeadersFromContext(ctx context.Context) http.Header {
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil || ginCtx.Request == nil {
		return nil
	}
	return ginCtx.Request.Header
}

func identityFingerprintAccount(auth *cliproxyauth.Auth) (accountKey string, authSubjectID string) {
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity != nil {
		return strings.TrimSpace(identity.ID), strings.TrimSpace(identity.ID)
	}
	if auth != nil {
		if id := strings.TrimSpace(auth.ID); id != "" {
			return id, ""
		}
		if idx := strings.TrimSpace(auth.EnsureIndex()); idx != "" {
			return idx, ""
		}
	}
	return "", ""
}

func observeRuntimeIdentityFingerprint(provider identityfingerprint.Provider, auth *cliproxyauth.Auth, ctx context.Context) *identityfingerprint.LearnedRecord {
	accountKey, authSubjectID := identityFingerprintAccount(auth)
	if accountKey == "" {
		return nil
	}
	headers := identityFingerprintHeadersFromContext(ctx)
	if len(headers) == 0 {
		record, err := usage.GetIdentityFingerprint(provider, accountKey)
		if err != nil {
			log.WithError(err).Warn("identity fingerprint: load learned record")
		}
		return record
	}
	record, _, err := usage.ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      provider,
		AccountKey:    accountKey,
		AuthSubjectID: authSubjectID,
		Headers:       headers.Clone(),
		ObservedAt:    time.Now().UTC(),
	})
	if err != nil {
		log.WithError(err).Warn("identity fingerprint: observe learned record")
		return nil
	}
	return record
}
