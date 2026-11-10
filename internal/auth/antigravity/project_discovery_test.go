package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCodeAssist answers loadCodeAssist and onboardUser the way CloudCode does,
// so discovery runs end to end without reaching Google.
type fakeCodeAssist struct {
	load       string
	onboard    func(poll int) string
	loads      atomic.Int32
	onboards   atomic.Int32
	lastTierID atomic.Value
}

func (f *fakeCodeAssist) client(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: discoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		body := ""
		switch req.URL.String() {
		case APIEndpoint + "/" + APIVersion + ":loadCodeAssist":
			f.loads.Add(1)
			body = f.load
		case APIEndpoint + "/" + APIVersion + ":onboardUser":
			poll := int(f.onboards.Add(1))
			var sent struct {
				TierID string `json:"tierId"`
			}
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &sent)
			f.lastTierID.Store(sent.TierID)
			if f.onboard == nil {
				t.Errorf("unexpected onboardUser call (poll %d)", poll)
				return nil, fmt.Errorf("onboardUser must not be called")
			}
			body = f.onboard(poll)
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func shortenOnboardPolls(t *testing.T) {
	t.Helper()
	previous := onboardPollInterval
	onboardPollInterval = time.Millisecond
	t.Cleanup(func() { onboardPollInterval = previous })
}

// What #1032 captured from the wrong User-Agent: HTTP 200, no project, the
// refusal only visible in ineligibleTiers. Discovery must name it and leave
// onboardUser alone — onboarding as a refused client was how those accounts
// ended up recorded as having no project at all.
func TestFetchAccountDetailsReportsUnsupportedClient(t *testing.T) {
	upstream := &fakeCodeAssist{load: `{
		"allowedTiers": [{"id": "standard-tier", "isDefault": true}],
		"ineligibleTiers": [{"reasonCode": "UNSUPPORTED_CLIENT", "reasonMessage": "This client is not supported.", "tierId": "free-tier"}]
	}`}

	_, err := NewAntigravityAuth(nil, upstream.client(t)).FetchAccountDetails(context.Background(), "test-token")
	if !errors.Is(err, ErrUnsupportedClient) {
		t.Fatalf("FetchAccountDetails error = %v, want ErrUnsupportedClient", err)
	}
	if !strings.Contains(err.Error(), "This client is not supported.") {
		t.Fatalf("error %q should carry Google's reason message", err)
	}
	if errors.Is(err, ErrNoProjectID) {
		t.Fatalf("error %q must not read as a missing project", err)
	}
	if !IsTerminalProjectProbeError(err) {
		t.Fatal("a refused client is Google's answer and should be recorded as one")
	}
	if got := upstream.onboards.Load(); got != 0 {
		t.Fatalf("onboardUser calls = %d, want 0", got)
	}
}

// onboardUser is a long-running operation. Running out of polls says nothing
// about the account, so it must not come back as "no project, no error".
func TestOnboardUserPollingExhaustedIsRetryable(t *testing.T) {
	shortenOnboardPolls(t)
	upstream := &fakeCodeAssist{onboard: func(int) string { return `{"done": false}` }}

	projectID, err := NewAntigravityAuth(nil, upstream.client(t)).OnboardUser(context.Background(), "test-token", "free-tier")
	if projectID != "" {
		t.Fatalf("project = %q, want empty", projectID)
	}
	if !errors.Is(err, ErrOnboardingPending) {
		t.Fatalf("OnboardUser error = %v, want ErrOnboardingPending", err)
	}
	if IsTerminalProjectProbeError(err) {
		t.Fatal("an unfinished onboarding must stay retryable")
	}
	if got := upstream.onboards.Load(); got != int32(onboardMaxPolls) {
		t.Fatalf("onboardUser polls = %d, want %d", got, onboardMaxPolls)
	}
}

func TestOnboardUserReturnsProjectOncePollingFinishes(t *testing.T) {
	shortenOnboardPolls(t)
	upstream := &fakeCodeAssist{onboard: func(poll int) string {
		if poll < 3 {
			return `{"done": false}`
		}
		return `{"done": true, "response": {"cloudaicompanionProject": {"id": "onboarded-project"}}}`
	}}

	projectID, err := NewAntigravityAuth(nil, upstream.client(t)).OnboardUser(context.Background(), "test-token", "free-tier")
	if err != nil {
		t.Fatalf("OnboardUser: %v", err)
	}
	if projectID != "onboarded-project" {
		t.Fatalf("project = %q, want onboarded-project", projectID)
	}
}

func TestOnboardUserStopsWaitingWhenCancelled(t *testing.T) {
	previous := onboardPollInterval
	onboardPollInterval = time.Hour
	t.Cleanup(func() { onboardPollInterval = previous })
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &fakeCodeAssist{onboard: func(int) string {
		cancel()
		return `{"done": false}`
	}}

	done := make(chan error, 1)
	go func() {
		_, err := NewAntigravityAuth(nil, upstream.client(t)).OnboardUser(ctx, "test-token", "free-tier")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OnboardUser error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnboardUser kept waiting between polls after its context ended")
	}
}

// When onboarding finishes without a project, Google's ineligibility reasons
// are the only clue to why; the error keeps them and still reads as terminal.
func TestFetchAccountDetailsKeepsIneligibleReasons(t *testing.T) {
	shortenOnboardPolls(t)
	upstream := &fakeCodeAssist{
		load: `{
			"allowedTiers": [{"id": "free-tier", "isDefault": true}],
			"ineligibleTiers": [{"reasonCode": "INELIGIBLE_ACCOUNT", "tierId": "standard-tier"}]
		}`,
		onboard: func(int) string { return `{"done": true, "response": {}}` },
	}

	_, err := NewAntigravityAuth(nil, upstream.client(t)).FetchAccountDetails(context.Background(), "test-token")
	if !errors.Is(err, ErrNoProjectID) {
		t.Fatalf("FetchAccountDetails error = %v, want ErrNoProjectID", err)
	}
	if !strings.Contains(err.Error(), "INELIGIBLE_ACCOUNT") {
		t.Fatalf("error %q should name Google's ineligibility reason", err)
	}
	if got, _ := upstream.lastTierID.Load().(string); got != "free-tier" {
		t.Fatalf("onboardUser tierId = %q, want the default allowed tier", got)
	}
}
