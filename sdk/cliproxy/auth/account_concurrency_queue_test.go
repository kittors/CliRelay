package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func limitedAuth(id string, limit int) *Auth {
	return &Auth{
		ID:         id,
		Attributes: map[string]string{"concurrency_limit": strconv.Itoa(limit)},
	}
}

func TestAcquireSlotWait_QueuesUntilSlotReleased(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-queue-1", 1)

	release, err := limiter.AcquireSlot(auth)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	type waitResult struct {
		release func()
		authID  string
		err     error
	}
	results := make(chan waitResult, 1)
	go func() {
		rel, id, errWait := limiter.AcquireSlotWait(context.Background(), []*Auth{auth}, 2*time.Second, 0)
		results <- waitResult{release: rel, authID: id, err: errWait}
	}()

	// The waiter must actually block instead of failing fast.
	select {
	case res := <-results:
		t.Fatalf("expected caller to queue, got early result: authID=%q err=%v", res.authID, res.err)
	case <-time.After(50 * time.Millisecond):
	}
	if depth := limiter.QueueDepth(auth.ID); depth != 1 {
		t.Fatalf("expected queue depth 1, got %d", depth)
	}
	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 1 {
		t.Fatalf("expected in-flight 1 while queued, got %d", inFlight)
	}

	release()

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("queued acquire failed: %v", res.err)
		}
		if res.authID != auth.ID {
			t.Fatalf("expected slot on %q, got %q", auth.ID, res.authID)
		}
		// The slot is handed over, never dropped: in-flight stays at the limit.
		if inFlight := limiter.GetInFlight(auth.ID); inFlight != 1 {
			t.Fatalf("expected in-flight 1 after handoff, got %d", inFlight)
		}
		res.release()
	case <-time.After(2 * time.Second):
		t.Fatal("queued caller was not granted the released slot")
	}

	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after final release, got %d", inFlight)
	}
	if depth := limiter.QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("expected empty queue, got depth %d", depth)
	}
}

func TestAcquireSlotWait_TimeoutReportsRateLimit(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-queue-timeout", 2)

	rel1, _ := limiter.AcquireSlot(auth)
	rel2, _ := limiter.AcquireSlot(auth)
	defer rel1()
	defer rel2()

	start := time.Now()
	release, authID, err := limiter.AcquireSlotWait(context.Background(), []*Auth{auth}, 80*time.Millisecond, 0)
	elapsed := time.Since(start)

	if err == nil {
		if release != nil {
			release()
		}
		t.Fatalf("expected timeout error, got slot on %q", authID)
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("returned before the timeout elapsed: %s", elapsed)
	}
	if !errors.Is(err, ErrAccountConcurrencyExceeded) {
		t.Fatalf("expected ErrAccountConcurrencyExceeded, got %v", err)
	}

	var concErr *AccountConcurrencyError
	if !errors.As(err, &concErr) {
		t.Fatalf("expected *AccountConcurrencyError, got %T", err)
	}
	if concErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", concErr.StatusCode())
	}
	if concErr.AuthID != auth.ID || concErr.Active != 2 || concErr.Limit != 2 {
		t.Fatalf("unexpected error detail: %+v", concErr)
	}
	if concErr.Waited <= 0 {
		t.Fatalf("expected recorded wait duration, got %s", concErr.Waited)
	}
	// statusCodeFromError drives the HTTP response, so the queue timeout must not
	// fall through to a generic 500.
	if status := statusCodeFromError(err); status != http.StatusTooManyRequests {
		t.Fatalf("expected statusCodeFromError 429, got %d", status)
	}
	if depth := limiter.QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("expected abandoned waiter to leave the queue, got depth %d", depth)
	}
}

func TestAcquireSlotWait_ContextCancellationStopsWaiting(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-queue-cancel", 1)

	release, _ := limiter.AcquireSlot(auth)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, errWait := limiter.AcquireSlotWait(ctx, []*Auth{auth}, time.Minute, 0)
		errCh <- errWait
	}()

	// Give the caller time to enqueue, then hang up like a disconnected client.
	waitForQueueDepth(t, limiter, auth.ID, 1)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller kept waiting")
	}
	if depth := limiter.QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("expected cancelled waiter to leave the queue, got depth %d", depth)
	}
}

func TestAcquireSlotWait_GrantsInFIFOOrder(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-queue-fifo", 1)

	release, _ := limiter.AcquireSlot(auth)

	const waiters = 4
	granted := make(chan int, waiters)
	releases := make(chan func(), waiters)
	for i := 0; i < waiters; i++ {
		// Enqueue one at a time so the expected order is unambiguous.
		go func(idx int) {
			rel, _, err := limiter.AcquireSlotWait(context.Background(), []*Auth{auth}, 5*time.Second, 0)
			if err != nil {
				granted <- -1
				return
			}
			granted <- idx
			releases <- rel
		}(i)
		waitForQueueDepth(t, limiter, auth.ID, i+1)
	}

	release()
	for i := 0; i < waiters; i++ {
		select {
		case idx := <-granted:
			if idx != i {
				t.Fatalf("expected waiter %d to be served next, got %d", i, idx)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("waiter %d never got a slot", i)
		}
		select {
		case rel := <-releases:
			rel()
		case <-time.After(5 * time.Second):
			t.Fatalf("waiter %d never reported its release func", i)
		}
	}

	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after draining the queue, got %d", inFlight)
	}
}

func TestAcquireSlotWait_ResumesOnWhicheverAccountFreesFirst(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	first := limitedAuth("auth-multi-1", 1)
	second := limitedAuth("auth-multi-2", 1)

	relFirst, _ := limiter.AcquireSlot(first)
	relSecond, _ := limiter.AcquireSlot(second)

	type waitResult struct {
		release func()
		authID  string
		err     error
	}
	results := make(chan waitResult, 1)
	go func() {
		rel, id, err := limiter.AcquireSlotWait(context.Background(), []*Auth{first, second}, 5*time.Second, 0)
		results <- waitResult{release: rel, authID: id, err: err}
	}()

	waitForQueueDepth(t, limiter, first.ID, 1)
	waitForQueueDepth(t, limiter, second.ID, 1)

	// Free the second account; the waiter must resume there, not wait for the first.
	relSecond()

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("multi-account wait failed: %v", res.err)
		}
		if res.authID != second.ID {
			t.Fatalf("expected slot on %q, got %q", second.ID, res.authID)
		}
		res.release()
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not resume on the account that freed a slot")
	}

	// The waiter must also be gone from the queue of the account it did not use,
	// otherwise that account would hand a slot to a caller that already left.
	if depth := limiter.QueueDepth(first.ID); depth != 0 {
		t.Fatalf("expected waiter removed from the other queue, got depth %d", depth)
	}
	relFirst()
	if inFlight := limiter.GetInFlight(first.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0 on the untouched account, got %d", inFlight)
	}
}

func TestAcquireSlotWait_ZeroTimeoutFailsFast(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-no-queue", 1)

	release, _ := limiter.AcquireSlot(auth)
	defer release()

	start := time.Now()
	rel, _, err := limiter.AcquireSlotWait(context.Background(), []*Auth{auth}, 0, 0)
	if err == nil {
		if rel != nil {
			rel()
		}
		t.Fatal("expected immediate failure when queuing is disabled")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fail-fast path blocked for %s", elapsed)
	}
	if !errors.Is(err, ErrAccountConcurrencyExceeded) {
		t.Fatalf("expected ErrAccountConcurrencyExceeded, got %v", err)
	}
	var concErr *AccountConcurrencyError
	if errors.As(err, &concErr) && concErr.Waited != 0 {
		t.Fatalf("expected no recorded wait when queuing is off, got %s", concErr.Waited)
	}
	if depth := limiter.QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("fail-fast path must not enqueue, got depth %d", depth)
	}
}

func TestAcquireSlotWait_RejectsWhenQueueIsFull(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-queue-depth", 1)

	release, _ := limiter.AcquireSlot(auth)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _, _ = limiter.AcquireSlotWait(ctx, []*Auth{auth}, time.Minute, 1)
	}()
	waitForQueueDepth(t, limiter, auth.ID, 1)

	rel, _, err := limiter.AcquireSlotWait(context.Background(), []*Auth{auth}, time.Minute, 1)
	if err == nil {
		if rel != nil {
			rel()
		}
		t.Fatal("expected rejection once the queue is full")
	}
	if !errors.Is(err, ErrAccountConcurrencyExceeded) {
		t.Fatalf("expected ErrAccountConcurrencyExceeded, got %v", err)
	}
	if depth := limiter.QueueDepth(auth.ID); depth != 1 {
		t.Fatalf("expected queue to stay at its cap, got depth %d", depth)
	}
}

func TestReleaseSlot_SkipsHandoffWhenLimitShrank(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-shrink", 2)

	rel1, _ := limiter.AcquireSlot(auth)
	rel2, _ := limiter.AcquireSlot(auth)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitErr := make(chan error, 1)
	go func() {
		_, _, err := limiter.AcquireSlotWait(ctx, []*Auth{auth}, 5*time.Second, 0)
		waitErr <- err
	}()
	waitForQueueDepth(t, limiter, auth.ID, 1)

	// Operator lowers the account limit while two requests are in flight.
	auth.Attributes["concurrency_limit"] = "1"

	// Releasing must drain toward the new limit rather than hand the slot on,
	// otherwise the account would stay above its configured ceiling forever.
	rel1()
	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 1 {
		t.Fatalf("expected in-flight to drop to 1, got %d", inFlight)
	}
	select {
	case err := <-waitErr:
		t.Fatalf("waiter was granted a slot above the lowered limit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Once in-flight fits the new limit the next release may hand over again.
	rel2()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("waiter failed after the account drained: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never resumed after the account drained to its new limit")
	}
}

func TestAcquireSlotWait_UnlimitedAccountNeverQueues(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := &Auth{ID: "auth-unlimited-queue"}

	releases := make([]func(), 0, 16)
	for i := 0; i < 16; i++ {
		rel, id, err := limiter.AcquireSlotWait(context.Background(), []*Auth{auth}, time.Millisecond, 0)
		if err != nil {
			t.Fatalf("unlimited account rejected acquire %d: %v", i, err)
		}
		if id != auth.ID {
			t.Fatalf("expected slot on %q, got %q", auth.ID, id)
		}
		releases = append(releases, rel)
	}
	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 16 {
		t.Fatalf("expected in-flight 16, got %d", inFlight)
	}
	for _, rel := range releases {
		rel()
	}
	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0, got %d", inFlight)
	}
}

// TestAcquireSlotWait_NoSlotLeakUnderCancelRace hammers the window where a slot is
// handed over at the same moment its waiter times out. Losing that race would
// strand in-flight capacity, which is exactly how an account ends up reporting
// active=N forever while nothing is running.
func TestAcquireSlotWait_NoSlotLeakUnderCancelRace(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := limitedAuth("auth-race", 2)

	const rounds = 300
	var wg sync.WaitGroup
	var granted atomic.Int64

	for i := 0; i < rounds; i++ {
		hold, err := limiter.AcquireSlot(auth)
		if err != nil {
			t.Fatalf("round %d: holder acquire failed: %v", i, err)
		}
		hold2, err := limiter.AcquireSlot(auth)
		if err != nil {
			t.Fatalf("round %d: second holder acquire failed: %v", i, err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			// A very short timeout lands the abandon() and tryGrant() paths in the
			// same instant often enough to expose a lost slot.
			rel, _, errWait := limiter.AcquireSlotWait(context.Background(), []*Auth{auth}, time.Millisecond, 0)
			if errWait == nil {
				granted.Add(1)
				rel()
			}
		}()

		time.Sleep(time.Millisecond)
		hold()
		hold2()
		wg.Wait()

		if inFlight := limiter.GetInFlight(auth.ID); inFlight != 0 {
			t.Fatalf("round %d: leaked %d in-flight slots", i, inFlight)
		}
		if depth := limiter.QueueDepth(auth.ID); depth != 0 {
			t.Fatalf("round %d: leaked %d queued waiters", i, depth)
		}
	}
	t.Logf("cancel race: %d/%d waiters won the handoff", granted.Load(), rounds)
}

// waitForQueueDepth blocks until the account queue reaches depth, so tests never
// race the goroutine they just started.
func waitForQueueDepth(t *testing.T, limiter *AccountConcurrencyLimiter, authID string, depth int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if limiter.QueueDepth(authID) >= depth {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue for %s never reached depth %d (currently %d)", authID, depth, limiter.QueueDepth(authID))
}
