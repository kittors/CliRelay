package refreshguard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ctxKey struct{}

func TestExchangeOutlivesItsCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "kept"))
	started := make(chan struct{})
	release := make(chan struct{})
	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		token, err := Exchange(ctx, "test", "rt-outlives", func(fctx context.Context) (string, error) {
			close(started)
			<-release
			if err := fctx.Err(); err != nil {
				return "", err
			}
			if fctx.Value(ctxKey{}) != "kept" {
				return "", errors.New("caller values were dropped")
			}
			if _, ok := fctx.Deadline(); !ok {
				return "", errors.New("the exchange must stay bounded")
			}
			return "rotated", nil
		})
		done <- result{token, err}
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("Exchange returned before the exchange finished; a rotated token would be dropped")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	got := <-done
	if got.err != nil || got.token != "rotated" {
		t.Fatalf("Exchange = %q, %v; want the rotated token after the caller gave up", got.token, got.err)
	}
}

func TestExchangeSharesOneCallPerToken(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = Exchange(context.Background(), "test", "rt-shared", func(context.Context) (string, error) {
				calls.Add(1)
				<-release
				return "rotated-once", nil
			})
		}(i)
	}
	// Let every caller reach the shared call before it completes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("exchange ran %d times for one refresh token; the extra ones present a retired token", n)
	}
	for i, got := range results {
		if got != "rotated-once" {
			t.Fatalf("caller %d got %q, want the shared result", i, got)
		}
	}
}

func TestExchangeKeepsTokensAndProvidersApart(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, pair := range [][2]string{{"codex", "rt-a"}, {"codex", "rt-b"}, {"claude", "rt-a"}} {
		wg.Add(1)
		go func(provider, token string) {
			defer wg.Done()
			_, _ = Exchange(context.Background(), provider, token, func(context.Context) (string, error) {
				calls.Add(1)
				<-release
				return token, nil
			})
		}(pair[0], pair[1])
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 3 {
		t.Fatalf("exchange ran %d times, want one per provider and token", n)
	}
}

func TestExchangeReturnsTheError(t *testing.T) {
	want := errors.New("invalid_grant")
	got, err := Exchange(context.Background(), "test", "rt-error", func(context.Context) (*struct{}, error) {
		return nil, want
	})
	if !errors.Is(err, want) || got != nil {
		t.Fatalf("Exchange = %v, %v; want nil, %v", got, err, want)
	}
}

func TestExchangeKeyDoesNotHoldTheToken(t *testing.T) {
	key := exchangeKey("codex", "rt-secret-value")
	if want := "codex:"; key[:len(want)] != want {
		t.Fatalf("key %q must start with the provider", key)
	}
	if strings.Contains(key, "rt-secret-value") {
		t.Fatalf("key %q contains the refresh token", key)
	}
}
