package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	c := newCoordinator(true, "node-a")
	ev, err := c.envelope(TopicAuth, AuthEvent{ID: "t/1.json", Version: 7})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeEnvelope(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "Resync") || strings.Contains(encoded, "resync") {
		t.Fatalf("Resync is local-only and must never be sent: %s", encoded)
	}
	decoded, err := decodeEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var payload AuthEvent
	if err := decoded.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if decoded.Topic != TopicAuth || decoded.Origin != "node-a" || payload.ID != "t/1.json" || payload.Version != 7 || decoded.Resync {
		t.Fatalf("round trip = %+v / %+v", decoded, payload)
	}
}

func TestEnvelopeRejectsOversizedNotify(t *testing.T) {
	// The payload check leaves headroom, but a huge node id or topic can
	// still push the envelope over PostgreSQL's NOTIFY limit.
	ev := Event{Topic: TopicAuth, Origin: strings.Repeat("n", maxNotifyBytes), Payload: []byte(`{}`)}
	if _, err := encodeEnvelope(ev); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversized envelope error = %v", err)
	}
}

func TestDecodeEnvelopeRejectsGarbage(t *testing.T) {
	for _, payload := range []string{"", "not json", `{"origin":"a"}`} {
		if _, err := decodeEnvelope(payload); err == nil {
			t.Errorf("decodeEnvelope(%q) accepted a message without topic", payload)
		}
	}
}

func TestListenerBackoffSequence(t *testing.T) {
	got := []time.Duration{time.Second}
	for len(got) < 7 {
		got = append(got, nextBackoff(got[len(got)-1], 15*time.Second))
	}
	want := []time.Duration{1, 2, 4, 8, 15, 15, 15}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff[%d] = %s, want %ds (sequence %v)", i, got[i], want[i], got)
		}
	}
}

func TestPrepareInstallsFollowerThatStartAdopts(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	c := Prepare(Options{Enabled: true, NodeID: "node-p"})
	if Default() != c || !c.Enabled() || c.IsLeader() || c.ActiveNodeCount() != 1 {
		t.Fatalf("prepared coordinator must be the default follower: enabled=%v leader=%v", c.Enabled(), c.IsLeader())
	}
	if err := c.Publish(context.Background(), TopicAuth, AuthEvent{ID: "x"}); err != nil {
		t.Fatalf("publish before Start must be a no-op: %v", err)
	}
	nodes, err := c.Nodes(context.Background())
	if err != nil || len(nodes) != 1 || !nodes[0].Self || nodes[0].Leader || nodes[0].NodeID != "node-p" {
		t.Fatalf("prepared node view = %+v, %v", nodes, err)
	}
	var got []Event
	c.Subscribe(TopicConfig, func(ev Event) { got = append(got, ev) })
	if adoptPrepared("other-node") != nil {
		t.Fatal("a different node id must not adopt the prepared coordinator")
	}
	adopted := adoptPrepared("node-p")
	if adopted != c {
		t.Fatal("Start must reuse the prepared coordinator")
	}
	if adoptPrepared("node-p") != nil {
		t.Fatal("a prepared coordinator is adopted once")
	}
	// Subscriptions made before Start stay attached.
	adopted.deliver(Event{Resync: true, Origin: "node-p"})
	if len(got) != 1 || !got[0].Resync {
		t.Fatalf("subscriber registered before Start got %+v", got)
	}
}

func TestPrepareDisabledKeepsSingleNode(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	SetDefault(nil)
	c := Prepare(Options{Enabled: false, NodeID: "solo"})
	if c.Enabled() || !c.IsLeader() || Default() != c {
		t.Fatalf("disabled Prepare must leave single-node behaviour alone")
	}
}

func TestClosedPreparedCoordinatorIsNotAdopted(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	c := Prepare(Options{Enabled: true, NodeID: "node-closed"})
	c.Close()
	if adoptPrepared("node-closed") != nil {
		t.Fatal("a closed coordinator must not be started")
	}
}

func TestTuneSessionAttemptsEverySetting(t *testing.T) {
	var names []string
	exec := func(_ context.Context, _ string, args ...any) error {
		name := args[0].(string)
		names = append(names, name)
		if name == "tcp_user_timeout" {
			return errors.New(`unrecognized configuration parameter "tcp_user_timeout"`)
		}
		return nil
	}
	err := tuneSession(context.Background(), exec, "clirelay-leader:a")
	if err == nil || !strings.Contains(err.Error(), "tcp_user_timeout") {
		t.Fatalf("first failure must be reported, got %v", err)
	}
	want := []string{"tcp_keepalives_idle", "tcp_keepalives_interval", "tcp_keepalives_count", "tcp_user_timeout", "application_name"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("settings applied = %v, want %v", names, want)
	}
	if len(sessionKeepalives) != 4 {
		t.Fatal("tuneSession must not grow the shared keepalive list")
	}
}
