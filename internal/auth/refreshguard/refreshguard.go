// Package refreshguard runs OAuth refresh-token exchanges so that a caller
// giving up cannot lose a rotated refresh token.
//
// Codex, Claude, Qwen, iFlow, Kimi and xAI hand out single-use refresh tokens:
// a successful exchange retires the token it was given and returns the only
// copy of its replacement. An exchange that follows its caller's context is cut
// off when that context ends (a client disconnecting, the refresh loop stopping
// at shutdown), and if the upstream had already rotated, the new token existed
// only in the response nobody read. The credential is left with a dead refresh
// token: the next refresh fails with refresh_token_reused and the account needs
// a new login. Two exchanges of the same token at once fail the same way, since
// the slower one presents a token the faster one just retired.
package refreshguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"golang.org/x/sync/singleflight"
)

// ExchangeTimeout bounds one exchange, retries included, once it no longer
// follows its caller. The HTTP clients involved time out each attempt after 30
// seconds; this only stops a hung exchange from holding its key forever.
const ExchangeTimeout = 2 * time.Minute

var exchanges singleflight.Group

// Exchange runs fn, the exchange of refreshToken with provider's token endpoint,
// so that it survives its caller:
//
//   - fn gets a context that keeps ctx's values but not its cancellation or
//     deadline, bounded by ExchangeTimeout instead.
//   - Exchange waits for fn even when ctx ends first, so the caller always gets
//     the result it has to store.
//   - Concurrent exchanges of the same token with the same provider share one
//     call to fn and its result.
//
// Callers must store the result without following ctx either; see
// context.WithoutCancel.
func Exchange[T any](ctx context.Context, provider, refreshToken string, fn func(context.Context) (T, error)) (T, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	value, err, _ := exchanges.Do(exchangeKey(provider, refreshToken), func() (any, error) {
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), ExchangeTimeout)
		defer cancel()
		return fn(detached)
	})
	result, _ := value.(T)
	return result, err
}

// exchangeKey keeps refresh tokens out of the in-flight map.
func exchangeKey(provider, refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return provider + ":" + hex.EncodeToString(sum[:])
}
