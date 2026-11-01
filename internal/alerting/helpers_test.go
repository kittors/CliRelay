package alerting

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// clock is a settable time source for the tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func timeNow() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }

// roundTripFunc answers requests without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

// recordedRequest is what a fake receiver saw.
type recordedRequest struct {
	URL    string
	Header http.Header
	Body   []byte
}

// fakeReceiver answers every request with a fixed reply and records them.
type fakeReceiver struct {
	mu       sync.Mutex
	status   int
	body     string
	err      error
	requests []recordedRequest
}

func (f *fakeReceiver) RoundTrip(r *http.Request) (*http.Response, error) {
	data, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{URL: r.URL.String(), Header: r.Header.Clone(), Body: data})
	status, body, err := f.status, f.body, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if status == 0 {
		status = http.StatusOK
	}
	return respond(status, body), nil
}

func (f *fakeReceiver) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// fakeSender records what the engine sends and answers per channel.
type fakeSender struct {
	mu      sync.Mutex
	sent    []Message
	trusted []bool
	// fail makes every delivery fail.
	fail bool
	// onSend runs before the delivery is answered.
	onSend func(Message)
}

func (f *fakeSender) Send(_ context.Context, channels []Channel, msg Message, trusted bool) []Delivery {
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.trusted = append(f.trusted, trusted)
	hook, fail := f.onSend, f.fail
	f.mu.Unlock()
	if hook != nil {
		hook(msg)
	}
	out := make([]Delivery, 0, len(channels))
	for _, ch := range channels {
		d := Delivery{ChannelID: ch.ID, ChannelName: ch.Name, Kind: ch.Kind, Status: DeliverySent}
		if fail {
			d.Status, d.Error = DeliveryFailed, "the request failed"
		}
		out = append(out, d)
	}
	return out
}

func (f *fakeSender) messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

// fakeSources serves canned samples and counts how often it is asked.
type fakeSources struct {
	mu          sync.Mutex
	accounts    map[string][]AccountSample
	spenders    map[string][]SpendSample
	accountErr  error
	accountCall int
	spendCall   int
	panicFor    string
}

func (f *fakeSources) Accounts(_ context.Context, tenantID string, _ time.Time) ([]AccountSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountCall++
	if f.panicFor == tenantID {
		panic("boom")
	}
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	return append([]AccountSample(nil), f.accounts[tenantID]...), nil
}

func (f *fakeSources) Spenders(_ context.Context, tenantID string, _ time.Time) ([]SpendSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spendCall++
	return append([]SpendSample(nil), f.spenders[tenantID]...), nil
}

func (f *fakeSources) setAccounts(tenantID string, accounts ...AccountSample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accounts == nil {
		f.accounts = map[string][]AccountSample{}
	}
	f.accounts[tenantID] = accounts
}

func (f *fakeSources) calls() (accounts, spenders int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountCall, f.spendCall
}
