package auth

import "strings"

// retryExclusionsMetadataKey carries what a caller learned from an attempt it
// abandoned: which accounts already failed this request, and why. It rides in
// the request metadata like the other scheduling hints because the Selector and
// ProviderExecutor contracts cannot grow parameters.
const retryExclusionsMetadataKey = "cliproxy.retry_exclusions"

type retryExclusions struct {
	authIDs []string
	cause   error
}

// ExcludeAuthsForRetry returns a copy of meta for running the request again
// without the given accounts. cause is the failure that made the caller retry.
//
// The scheduler skips the excluded accounts, and when nothing else can serve
// the request it reports cause instead of a generic "no auth available": the
// client should learn what actually went wrong upstream, not that the retry
// found the pool empty.
//
// A pinned or otherwise single-pick request has exactly one account, so for
// those the accounts are not excluded and the retry may reach the same account
// again, as it always could.
//
// meta is copied rather than written to: the executor of the abandoned stream
// may still be reading its metadata while the retry is being scheduled.
func ExcludeAuthsForRetry(meta map[string]any, authIDs []string, cause error) map[string]any {
	out := make(map[string]any, len(meta)+1)
	for key, value := range meta {
		out[key] = value
	}
	ids := make([]string, 0, len(authIDs))
	seen := make(map[string]struct{}, len(authIDs))
	for _, id := range authIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	out[retryExclusionsMetadataKey] = retryExclusions{authIDs: ids, cause: cause}
	return out
}

func retryExclusionsFromMetadata(meta map[string]any) (retryExclusions, bool) {
	if len(meta) == 0 {
		return retryExclusions{}, false
	}
	exclusions, ok := meta[retryExclusionsMetadataKey].(retryExclusions)
	return exclusions, ok
}

// applyRetryExclusions seeds a fresh execution scope with what earlier attempts
// of the same request learned.
func applyRetryExclusions(scope *mixedExecutionScope) {
	exclusions, ok := retryExclusionsFromMetadata(scope.opts.Metadata)
	if !ok {
		return
	}
	scope.lastErr = exclusions.cause
	if scope.singlePickRoute {
		return
	}
	for _, id := range exclusions.authIDs {
		scope.tried[id] = struct{}{}
	}
}
