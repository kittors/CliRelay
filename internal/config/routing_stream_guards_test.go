package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Both guards are opt-in: a group that sets neither behaves as it did before
// they existed. Busy waiting in particular holds client connections open, so a
// group must ask for it.
func TestChannelGroupStreamGuardDefaults(t *testing.T) {
	var group RoutingChannelGroup
	if got := group.FirstTokenTimeout(); got != 0 {
		t.Fatalf("unset first-token timeout = %s, want off", got)
	}
	if got := group.BusyWaitMax(); got != 0 {
		t.Fatalf("unset busy wait = %s, want off", got)
	}
	group.BusyWaitMaxSeconds = new(0)
	if got := group.BusyWaitMax(); got != 0 {
		t.Fatalf("explicit 0 busy wait = %s, want off", got)
	}
	group.FirstTokenTimeoutSeconds = 30
	group.BusyWaitMaxSeconds = new(90)
	if group.FirstTokenTimeout() != 30*time.Second || group.BusyWaitMax() != 90*time.Second {
		t.Fatalf("explicit values not used: %s, %s", group.FirstTokenTimeout(), group.BusyWaitMax())
	}
}

// The management API rejects values out of range instead of letting
// SanitizeRouting quietly change what the operator typed.
func TestValidateRoutingStreamGuards(t *testing.T) {
	valid := []RoutingChannelGroup{
		{Name: "off"},
		{Name: "min", FirstTokenTimeoutSeconds: MinFirstTokenTimeoutSeconds, BusyWaitMaxSeconds: new(0)},
		{Name: "max", FirstTokenTimeoutSeconds: MaxFirstTokenTimeoutSeconds, BusyWaitMaxSeconds: new(MaxBusyWaitMaxSeconds)},
	}
	if err := ValidateRoutingStreamGuards(RoutingConfig{ChannelGroups: valid}); err != nil {
		t.Fatalf("valid groups rejected: %v", err)
	}
	invalid := []RoutingChannelGroup{
		{Name: "too-short", FirstTokenTimeoutSeconds: MinFirstTokenTimeoutSeconds - 1},
		{Name: "too-long", FirstTokenTimeoutSeconds: MaxFirstTokenTimeoutSeconds + 1},
		{Name: "negative-timeout", FirstTokenTimeoutSeconds: -1},
		{Name: "wait-too-long", BusyWaitMaxSeconds: new(MaxBusyWaitMaxSeconds + 1)},
		{Name: "negative-wait", BusyWaitMaxSeconds: new(-1)},
	}
	for _, group := range invalid {
		err := ValidateRoutingStreamGuards(RoutingConfig{ChannelGroups: []RoutingChannelGroup{group}})
		if err == nil || !strings.Contains(err.Error(), group.Name) {
			t.Fatalf("group %q: err = %v, want a rejection naming the group", group.Name, err)
		}
	}
}

// Configs that bypass the management API (imports, older panels, rows written
// by hand) are brought into range; a negative value is read as off, never as
// "wait forever".
func TestSanitizeRoutingClampsStreamGuards(t *testing.T) {
	got := NormalizeRoutingConfig(RoutingConfig{ChannelGroups: []RoutingChannelGroup{
		{Name: "a", FirstTokenTimeoutSeconds: -4, BusyWaitMaxSeconds: new(-4)},
		{Name: "b", FirstTokenTimeoutSeconds: 2, BusyWaitMaxSeconds: new(900)},
		{Name: "c", FirstTokenTimeoutSeconds: 900},
		{Name: "d", FirstTokenTimeoutSeconds: 45, BusyWaitMaxSeconds: new(0)},
	}})
	want := []struct {
		timeout int
		wait    *int
	}{
		{0, new(0)},
		{MinFirstTokenTimeoutSeconds, new(MaxBusyWaitMaxSeconds)},
		{MaxFirstTokenTimeoutSeconds, nil},
		{45, new(0)},
	}
	for i, group := range got.ChannelGroups {
		if group.FirstTokenTimeoutSeconds != want[i].timeout {
			t.Fatalf("group %s timeout = %d, want %d", group.Name, group.FirstTokenTimeoutSeconds, want[i].timeout)
		}
		switch {
		case want[i].wait == nil && group.BusyWaitMaxSeconds != nil:
			t.Fatalf("group %s busy wait = %d, want unset", group.Name, *group.BusyWaitMaxSeconds)
		case want[i].wait != nil && (group.BusyWaitMaxSeconds == nil || *group.BusyWaitMaxSeconds != *want[i].wait):
			t.Fatalf("group %s busy wait = %v, want %d", group.Name, group.BusyWaitMaxSeconds, *want[i].wait)
		}
	}
}

// Unset and 0 behave alike for busy-wait-max-seconds (both are off), but the
// stored form, the JSON the panel reads and the comparison used to detect a
// change keep them apart: clearing the field must delete the key, and a 0 the
// operator typed must survive a save, so that what is stored stays what was
// entered and does not depend on what the default happens to be.
func TestBusyWaitUnsetAndZeroStayDistinct(t *testing.T) {
	unset := RoutingConfig{ChannelGroups: []RoutingChannelGroup{{Name: "team"}}}
	off := RoutingConfig{ChannelGroups: []RoutingChannelGroup{{Name: "team", BusyWaitMaxSeconds: new(0)}}}
	if SameRoutingConfig(unset, off) {
		t.Fatal("unset and 0 compare equal; switching the wait off would not be saved")
	}
	encoded, err := json.Marshal(NormalizeRoutingConfig(off))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"busy-wait-max-seconds":0`) {
		t.Fatalf("explicit 0 lost in %s", encoded)
	}
	var decoded RoutingConfig
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !SameRoutingConfig(decoded, off) {
		t.Fatalf("JSON round trip changed the config: %s", encoded)
	}
}
