package alerting

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrTooManyTests is returned when a tenant sends test messages faster than a
// person could be checking them. The test button posts to an address the tenant
// chose, so it is limited like any other thing that makes the server call out.
var ErrTooManyTests = errors.New("too many test messages; wait a moment and try again")

const (
	testMax    = 10
	testWindow = 5 * time.Minute
	// testBudget bounds one test request: one delivery timeout plus slack.
	testBudget = deliveryTimeout + 5*time.Second
	// updateAttempts is how many times a settings write retries after losing a
	// race with another writer before reporting the conflict.
	updateAttempts = 4
)

// Service is what the management API calls: read and write one tenant's
// settings, try a channel, read the history.
type Service struct {
	store  Store
	sender Sender
	now    func() time.Time

	mu    sync.Mutex
	tests map[string][]time.Time
}

// NewService builds the service. Both arguments are required.
func NewService(store Store, sender Sender) *Service {
	return &Service{store: store, sender: sender, now: time.Now, tests: map[string][]time.Time{}}
}

// Persistent reports whether writes are saved.
func (s *Service) Persistent() bool { return s != nil && s.store != nil && s.store.Persistent() }

// Settings returns the tenant's stored settings, or the defaults for a tenant
// that never saved any.
func (s *Service) Settings(tenantID string) (Settings, error) {
	settings, _, found, err := s.store.LoadSettings(tenantID)
	if err != nil {
		return Settings{}, err
	}
	if !found {
		return DefaultSettings(), nil
	}
	return settings.Normalized(), nil
}

// View returns the settings as the API shows them: secrets masked.
func (s *Service) View(tenantID string) (SettingsView, error) {
	settings, err := s.Settings(tenantID)
	if err != nil {
		return SettingsView{}, err
	}
	return settings.View(s.Persistent()), nil
}

// Update applies a write and returns the result as the API shows it. A write
// that is invalid returns a *FieldError and changes nothing.
func (s *Service) Update(tenantID string, patch Patch) (SettingsView, error) {
	if !s.Persistent() {
		return SettingsView{}, ErrNotPersistent
	}
	allowPrivate := TrustedTenant(tenantID)
	var lastErr error
	for attempt := 1; attempt <= updateAttempts; attempt++ {
		current, version, found, err := s.store.LoadSettings(tenantID)
		if err != nil {
			return SettingsView{}, err
		}
		if !found {
			current = DefaultSettings()
		}
		next, ferr := current.ApplyPatch(patch, s.now())
		if ferr != nil {
			return SettingsView{}, ferr
		}
		if ferr := next.Validate(allowPrivate); ferr != nil {
			return SettingsView{}, ferr
		}
		// The patch is re-applied to what is stored on every attempt, so losing a
		// race to another administrator's edit loses neither edit.
		if err = s.store.SaveSettings(tenantID, next, version); err == nil {
			return next.View(true), nil
		}
		if !errors.Is(err, ErrConflict) {
			return SettingsView{}, err
		}
		lastErr = err
		retrySleep(attempt)
	}
	return SettingsView{}, lastErr
}

// TestRequest names what to try: nothing (every enabled channel), a saved
// channel, or a draft that is not saved yet.
type TestRequest struct {
	ChannelID string        `json:"channel_id"`
	Channel   *ChannelPatch `json:"channel"`
}

// TestResult is the outcome of trying one channel.
type TestResult struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Kind        string `json:"kind"`
	OK          bool   `json:"ok"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	DurationMS  int64  `json:"duration_ms"`
}

// TestResponse is the answer of a test request.
type TestResponse struct {
	Results []TestResult `json:"results"`
}

// Test sends a test message to the channels the request names. It sends
// synchronously, because the operator is waiting to see whether it arrived, and
// records the message in the history like any other.
func (s *Service) Test(ctx context.Context, tenantID string, req TestRequest) (TestResponse, error) {
	settings, err := s.Settings(tenantID)
	if err != nil {
		return TestResponse{}, err
	}
	allowPrivate := TrustedTenant(tenantID)
	channels, err := s.testChannels(settings, req, allowPrivate)
	if err != nil {
		return TestResponse{}, err
	}
	if !s.allowTest(tenantID) {
		return TestResponse{}, ErrTooManyTests
	}

	now := s.now()
	msg := testMessage(tenantID, settings.Language, now, channels)
	ctx, cancel := context.WithTimeout(ctx, testBudget)
	defer cancel()
	deliveries := s.sender.Send(ctx, channels, msg, allowPrivate)

	response := TestResponse{Results: make([]TestResult, 0, len(deliveries))}
	for _, d := range deliveries {
		response.Results = append(response.Results, TestResult{
			ChannelID: d.ChannelID, ChannelName: d.ChannelName, Kind: d.Kind, OK: d.OK(),
			Status: d.Status, Error: d.Error, DurationMS: d.Duration.Milliseconds(),
		})
	}
	// The history is a convenience; a test that was delivered is not undone by
	// failing to write its line.
	_ = s.store.AppendEvents(tenantID, []Event{eventFor(msg, deliveries)})
	return response, nil
}

func (s *Service) testChannels(settings Settings, req TestRequest, allowPrivate bool) ([]Channel, error) {
	switch {
	case req.Channel != nil:
		draft := *req.Channel
		if strings.TrimSpace(draft.ID) == "" {
			draft.ID = ""
		}
		channel, ferr := settings.ChannelFromPatch(draft)
		if ferr != nil {
			return nil, ferr
		}
		if ferr := channel.validate("channel", allowPrivate); ferr != nil {
			return nil, ferr
		}
		if channel.ID == "" {
			// A draft has no id of its own; the result still needs one to be told apart.
			channel.ID = "draft"
		}
		return []Channel{channel}, nil
	case strings.TrimSpace(req.ChannelID) != "":
		for _, channel := range settings.Channels {
			if channel.ID == strings.TrimSpace(req.ChannelID) {
				if ferr := channel.validate("channel", allowPrivate); ferr != nil {
					return nil, ferr
				}
				return []Channel{channel}, nil
			}
		}
		return nil, fieldErr("channel_id", "no such channel")
	default:
		channels := settings.ActiveChannels()
		if len(channels) == 0 {
			return nil, fieldErr("channels", "there is no enabled channel to send to")
		}
		return channels, nil
	}
}

// allowTest counts a test against the tenant's allowance.
func (s *Service) allowTest(tenantID string) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := s.tests[tenantID][:0:0]
	for _, at := range s.tests[tenantID] {
		if now.Sub(at) < testWindow {
			recent = append(recent, at)
		}
	}
	if len(recent) >= testMax {
		s.tests[tenantID] = recent
		return false
	}
	s.tests[tenantID] = append(recent, now)
	return true
}

func testMessage(tenantID, language string, now time.Time, channels []Channel) Message {
	subject := Subject{Kind: SubjectChannel, Name: "test"}
	if len(channels) == 1 {
		subject.ID, subject.Name = channels[0].ID, cleanText(channels[0].Name)
	}
	text := pick(language,
		"If you can read this, the channel works. Nothing is wrong.",
		"如果你看到这条消息，说明该渠道可用。没有任何异常。")
	return Message{
		Version: payloadVersion, ID: uuid.NewString(), Time: now.UTC(), TenantID: tenantID,
		Rule: RuleTest, Severity: SeverityInfo, Language: language,
		Title: ruleTitle(RuleTest, language, 1), Text: text, Subject: subject,
		Items: []Item{{Subject: subject, Text: text}},
	}
}

// EventsResponse is the history as the API returns it.
type EventsResponse struct {
	Items []Event `json:"items"`
	Total int     `json:"total"`
}

// Events returns the newest entries of the history. A limit below 1 means the
// default (50); it is capped at what is kept.
func (s *Service) Events(tenantID string, limit int) (EventsResponse, error) {
	if limit < 1 {
		limit = 50
	}
	limit = min(limit, MaxEvents)
	all, err := s.store.LoadEvents(tenantID)
	if err != nil {
		return EventsResponse{}, err
	}
	items := all
	if len(items) > limit {
		items = items[:limit]
	}
	if items == nil {
		items = []Event{}
	}
	return EventsResponse{Items: items, Total: len(all)}, nil
}
