package alerting

import (
	"sort"
	"time"
)

const (
	// MaxEvents bounds the history of one tenant. It is a log for answering "did
	// that alert go out, and where", not an archive.
	MaxEvents = 200

	// maxSentKeys bounds the dedupe table of one tenant, so a tenant with an
	// enormous number of keys cannot grow its settings row without limit.
	maxSentKeys = 2048

	// minMessageGap is the least time between two messages of the same rule to the
	// same tenant. Problems that arrive a few minutes apart are folded into the
	// next message instead of each getting their own: a channel that buzzes every
	// minute is a channel people mute.
	minMessageGap = 10 * time.Minute

	// claimLease is how long a problem stays reserved while its message is being
	// delivered. If the process dies mid-delivery the reservation expires and the
	// next round sends the message; if two processes (the two backend slots during
	// a deploy) evaluate at once, only the one that wrote the reservation first
	// sends it.
	claimLease = 5 * time.Minute

	// failedRetryAfter is how long a message that no channel accepted waits before
	// it is tried again.
	failedRetryAfter = 15 * time.Minute
)

// Event is one entry of the alert history.
type Event struct {
	ID         string          `json:"id"`
	At         time.Time       `json:"at"`
	Rule       string          `json:"rule"`
	Severity   string          `json:"severity"`
	Subject    Subject         `json:"subject"`
	Title      string          `json:"title"`
	Text       string          `json:"text"`
	Deliveries []EventDelivery `json:"deliveries"`
}

// EventDelivery is what happened to an event on one channel.
type EventDelivery struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Error       string `json:"error"`
}

// eventFor records what became of a message.
func eventFor(msg Message, deliveries []Delivery) Event {
	event := Event{
		ID: msg.ID, At: msg.Time, Rule: msg.Rule, Severity: msg.Severity,
		Subject: msg.Subject, Title: msg.Title, Text: msg.Text,
		Deliveries: make([]EventDelivery, 0, len(deliveries)),
	}
	for _, d := range deliveries {
		event.Deliveries = append(event.Deliveries, EventDelivery{
			ChannelID: d.ChannelID, ChannelName: d.ChannelName, Kind: d.Kind, Status: d.Status, Error: d.Error,
		})
	}
	return event
}

// EventLog is the stored form of the history, newest first.
type EventLog struct {
	Items []Event `json:"items"`
}

// Merged returns the log with events added at the front and the oldest dropped
// beyond MaxEvents. An event already present (same id) is not added twice, so a
// retried write does not double an entry.
func (l EventLog) Merged(events []Event) EventLog {
	seen := make(map[string]struct{}, len(l.Items)+len(events))
	items := make([]Event, 0, len(l.Items)+len(events))
	add := func(batch []Event) {
		for _, event := range batch {
			if _, dup := seen[event.ID]; dup {
				continue
			}
			seen[event.ID] = struct{}{}
			items = append(items, event)
		}
	}
	add(events)
	add(l.Items)
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	if len(items) > MaxEvents {
		items = items[:MaxEvents]
	}
	return EventLog{Items: items}
}

// SentRecord says that a problem was reported, and until when it is not
// reported again.
type SentRecord struct {
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
	// Pending marks a reservation made before delivery (see claimLease).
	Pending bool `json:"pending,omitempty"`
	// Failed marks a problem whose message no channel accepted; Until is then the
	// retry time rather than the end of a quiet period.
	Failed bool `json:"failed,omitempty"`
}

// SentState is which problems were already reported. It is the memory that makes
// an alert a message and not a drumbeat.
type SentState struct {
	Records map[string]SentRecord `json:"records"`
}

// quiet reports whether key must not be reported at now.
func (s SentState) quiet(key string, now time.Time) bool {
	record, ok := s.Records[key]
	return ok && record.Until.After(now)
}

// set records key, creating the table when the state is new.
func (s *SentState) set(key string, record SentRecord) {
	if s.Records == nil {
		s.Records = make(map[string]SentRecord)
	}
	s.Records[key] = record
}

// prune drops what no longer suppresses anything, then enforces the size bound
// by forgetting the oldest records. A record that has expired is only history:
// the problem it stood for is reported again if it is still there.
func (s *SentState) prune(now time.Time) {
	for key, record := range s.Records {
		if !record.Until.After(now) {
			delete(s.Records, key)
		}
	}
	if len(s.Records) <= maxSentKeys {
		return
	}
	keys := make([]string, 0, len(s.Records))
	for key := range s.Records {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return s.Records[keys[i]].At.Before(s.Records[keys[j]].At) })
	for _, key := range keys[:len(keys)-maxSentKeys] {
		delete(s.Records, key)
	}
}

// forgetFailures drops the records of failure alerts whose subject is no longer
// failing, so that the next time it fails it is reported at once instead of
// waiting out the cooldown of the last episode. active holds the dedupe keys of
// everything that is failing now.
func (s *SentState) forgetFailures(prefix string, active map[string]struct{}) {
	for key := range s.Records {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			if _, failing := active[key]; !failing {
				delete(s.Records, key)
			}
		}
	}
}

// gapKey is the record that spaces the messages of one rule.
func gapKey(rule string) string { return "gap|" + rule }
