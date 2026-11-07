package codex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rotatingTokenEndpoint answers refresh requests like auth.openai.com: every
// success rotates the refresh token. It blocks each request until release is
// closed and aborts when the request's context ends, as a real transport does.
type rotatingTokenEndpoint struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func newRotatingTokenEndpoint() *rotatingTokenEndpoint {
	return &rotatingTokenEndpoint{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (e *rotatingTokenEndpoint) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		e.calls.Add(1)
		e.entered <- struct{}{}
		select {
		case <-e.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		body := `{"access_token":"at-new","refresh_token":"rt-new","id_token":"","token_type":"Bearer","expires_in":3600}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
}

func TestRefreshTokensFinishesAfterTheCallerGivesUp(t *testing.T) {
	endpoint := newRotatingTokenEndpoint()
	auth := &CodexAuth{httpClient: endpoint.client()}
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		data *CodexTokenData
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := auth.RefreshTokensWithRetry(ctx, "rt-caller-gives-up", 3)
		done <- result{data, err}
	}()
	<-endpoint.entered
	cancel() // e.g. shutdown stops the refresh loop
	close(endpoint.release)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("refresh failed after the caller gave up: %v; the rotated token would be lost", got.err)
		}
		if got.data.RefreshToken != "rt-new" {
			t.Fatalf("refresh token = %q, want the rotated one", got.data.RefreshToken)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not return")
	}
	if n := endpoint.calls.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times, want 1", n)
	}
}

func TestConcurrentRefreshesOfOneTokenSendOneRequest(t *testing.T) {
	endpoint := newRotatingTokenEndpoint()
	auth := &CodexAuth{httpClient: endpoint.client()}
	var wg sync.WaitGroup
	results := make([]*CodexTokenData, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			// The background loop retries; a management call refreshes once.
			if i%2 == 0 {
				results[i], err = auth.RefreshTokensWithRetry(context.Background(), "rt-shared", 3)
			} else {
				results[i], err = auth.RefreshTokens(context.Background(), "rt-shared")
			}
			if err != nil {
				t.Errorf("refresh %d: %v", i, err)
			}
		}(i)
	}
	<-endpoint.entered
	// Give the other callers time to join the exchange in flight.
	time.Sleep(50 * time.Millisecond)
	close(endpoint.release)
	wg.Wait()
	if n := endpoint.calls.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times for one refresh token; every call after the first presents a retired token", n)
	}
	for i, data := range results {
		if data == nil || data.RefreshToken != "rt-new" {
			t.Fatalf("caller %d got %+v, want the shared rotated token", i, data)
		}
	}
}
