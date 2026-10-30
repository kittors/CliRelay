package handlers

import (
	"context"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// defaultStreamingBootstrapRetries is how many times a stream that fails before
// its first byte may move to another account when streaming.bootstrap-retries
// is not configured.
//
// Two, because one retry covers an isolated bad connection or a single
// unhealthy account, and a second covers a brief fault shared by a couple of
// accounts (one upstream region, one proxy). A third failure in a row is rarely
// a property of the account any more, and retrying further would only add
// latency before the client sees the error. Each retry excludes every account
// that already failed this request, so the real bound is also the number of
// candidates: a two-account pool gets at most one retry.
const defaultStreamingBootstrapRetries = 2

// StreamingBootstrapRetries returns how many times a streaming request may move
// to another account before any byte has been sent to the client. An unset
// value takes the default; an explicit 0 (or a negative value) turns it off.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil && cfg.Streaming.BootstrapRetries != nil {
		retries = *cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// streamFailover moves a stream that failed before its first byte to another
// account. It remembers every account that already failed the request so a
// retry never lands on one of them again.
type streamFailover struct {
	h        *BaseAPIHandler
	servedBy string
	failed   []string
}

func newStreamFailover(h *BaseAPIHandler, servedBy string) *streamFailover {
	return &streamFailover{h: h, servedBy: strings.TrimSpace(servedBy)}
}

// retry starts the request again without the accounts that failed it so far.
// cause is the failure being retried; when no other account can take the
// request the auth manager returns it unchanged, so the client sees what
// actually went wrong rather than an empty pool.
func (f *streamFailover) retry(
	ctx context.Context,
	providers []string,
	req coreexecutor.Request,
	opts coreexecutor.Options,
	cause error,
	retry int,
	maxRetries int,
) (*coreexecutor.StreamResult, error) {
	if f.servedBy != "" {
		f.failed = append(f.failed, f.servedBy)
	}
	f.note(ctx, cause, retry, maxRetries)
	retryOpts := opts
	retryOpts.Metadata = coreauth.ExcludeAuthsForRetry(opts.Metadata, f.failed, cause)
	result, err := f.h.AuthManager.ExecuteStream(ctx, providers, req, retryOpts)
	if err != nil {
		return nil, err
	}
	f.servedBy = strings.TrimSpace(result.AuthID)
	return result, nil
}

// note records the abandoned attempt in the request log next to the upstream
// exchange the executors already captured, so a request that succeeded on its
// second account still shows why the first one was dropped.
func (f *streamFailover) note(ctx context.Context, cause error, retry int, maxRetries int) {
	if f.h == nil || f.h.Cfg == nil || !f.h.Cfg.RequestLog {
		return
	}
	account := f.servedBy
	if account == "" {
		account = "unknown"
	}
	reason := "unknown error"
	if cause != nil {
		reason = cause.Error()
	}
	appendAPIResponse(ginContextFrom(ctx), []byte(fmt.Sprintf(
		"[stream failover] account %s failed before any byte reached the client (status %d: %s); retrying on another account (%d of %d)",
		account, statusFromError(cause), reason, retry, maxRetries,
	)))
}
