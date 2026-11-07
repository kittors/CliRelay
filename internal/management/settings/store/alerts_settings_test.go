package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	_ "modernc.org/sqlite"
)

const (
	tenantA = "00000000-0000-0000-0000-00000000000a"
	tenantB = "00000000-0000-0000-0000-00000000000b"
)

func initAlertTestDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func sampleSettings(name string) alerting.Settings {
	s := alerting.DefaultSettings()
	s.Enabled = true
	s.Channels = []alerting.Channel{{ID: "ch-1", Name: name, Kind: alerting.KindWebhook, Enabled: true, URL: "https://hooks.example.com/alerts"}}
	return s
}

func TestAlertSettingsVersionedRoundTrip(t *testing.T) {
	initAlertTestDB(t)
	store := AlertSettings{}
	if !store.Persistent() {
		t.Fatal("a store on an open database must report itself persistent")
	}

	if _, version, found, err := store.LoadSettings(tenantA); err != nil || found || version != 0 {
		t.Fatalf("empty: version=%d found=%v err=%v, want version 0, not found", version, found, err)
	}
	if err := store.SaveSettings(tenantA, sampleSettings("first"), 0); err != nil {
		t.Fatalf("first save: %v", err)
	}
	got, version, found, err := store.LoadSettings(tenantA)
	if err != nil || !found || version < 1 {
		t.Fatalf("after save: version=%d found=%v err=%v", version, found, err)
	}
	if len(got.Channels) != 1 || got.Channels[0].Name != "first" || got.Channels[0].URL != "https://hooks.example.com/alerts" {
		t.Fatalf("stored settings = %+v", got)
	}

	// A writer that read the row before someone else saved it must be refused.
	if err := store.SaveSettings(tenantA, sampleSettings("stale"), 0); !errors.Is(err, alerting.ErrConflict) {
		t.Fatalf("stale write err = %v, want alerting.ErrConflict", err)
	}
	if err := store.SaveSettings(tenantA, sampleSettings("second"), version); err != nil {
		t.Fatalf("save at the read version: %v", err)
	}
	if err := store.SaveSettings(tenantA, sampleSettings("forced"), alerting.AnyVersion); err != nil {
		t.Fatalf("unchecked save: %v", err)
	}
	got, _, _, _ = store.LoadSettings(tenantA)
	if got.Channels[0].Name != "forced" {
		t.Fatalf("settings = %+v, want the forced write to win", got)
	}
}

func TestAlertSentStateVersionedRoundTrip(t *testing.T) {
	initAlertTestDB(t)
	store := AlertSettings{}
	state, version, err := store.LoadSent(tenantA)
	if err != nil || version != 0 || len(state.Records) != 0 {
		t.Fatalf("empty: state=%+v version=%d err=%v", state, version, err)
	}
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := store.SaveSent(tenantA, alerting.SentState{Records: map[string]alerting.SentRecord{"k": {At: until.Add(-time.Hour), Until: until, Pending: true}}}, 0); err != nil {
		t.Fatal(err)
	}
	state, version, err = store.LoadSent(tenantA)
	if err != nil || version < 1 || !state.Records["k"].Pending || !state.Records["k"].Until.Equal(until) {
		t.Fatalf("stored = %+v version=%d err=%v", state, version, err)
	}
	// Two engines (the two backend slots of a deploy) claiming the same problem:
	// the second still believes the row is absent and loses.
	if err := store.SaveSent(tenantA, alerting.SentState{}, 0); !errors.Is(err, alerting.ErrConflict) {
		t.Fatalf("a claim made against 'absent' after the row exists must conflict, got %v", err)
	}
	if err := store.SaveSent(tenantA, alerting.SentState{}, version); err != nil {
		t.Fatalf("a claim made against the current version must succeed: %v", err)
	}
}

func TestAlertRowsAreAnnouncedUnderTheirOwnDomain(t *testing.T) {
	initAlertTestDB(t)
	var mu sync.Mutex
	var events []cluster.ConfigEvent
	restore := configsync.SetPublisherForTest(func(_ context.Context, tx *sql.Tx, ev cluster.ConfigEvent) error {
		if tx == nil {
			return errors.New("published outside the write transaction")
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return nil
	})
	t.Cleanup(restore)

	store := AlertSettings{}
	if err := store.SaveSettings(tenantA, sampleSettings("x"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSent(tenantA, alerting.SentState{Records: map[string]alerting.SentRecord{"k": {}}}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents(tenantA, []alerting.Event{{ID: "e1", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("announcements = %+v, want one per write", events)
	}
	for _, ev := range events {
		// On the default domain every history entry would make the other nodes
		// reload the whole runtime configuration.
		if ev.Domain != configsync.DomainAlerts {
			t.Fatalf("announced under %q, want %q: %+v", ev.Domain, configsync.DomainAlerts, ev)
		}
		if ev.TenantID != tenantA {
			t.Fatalf("announced for tenant %q, want %q", ev.TenantID, tenantA)
		}
	}
}

func TestAlertRowsOfDifferentTenantsAreSeparate(t *testing.T) {
	initAlertTestDB(t)
	store := AlertSettings{}
	if err := store.SaveSettings(tenantA, sampleSettings("tenant a"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSettings(tenantB, sampleSettings("tenant b"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents(tenantA, []alerting.Event{{ID: "a1", At: time.Now(), Title: "for a"}}); err != nil {
		t.Fatal(err)
	}

	a, _, _, _ := store.LoadSettings(tenantA)
	b, _, _, _ := store.LoadSettings(tenantB)
	if a.Channels[0].Name != "tenant a" || b.Channels[0].Name != "tenant b" {
		t.Fatalf("settings leaked across tenants: a=%+v b=%+v", a, b)
	}
	if events, err := store.LoadEvents(tenantB); err != nil || len(events) != 0 {
		t.Fatalf("tenant b sees tenant a's history: %+v err=%v", events, err)
	}

	all, err := store.SettingsOfAll()
	if err != nil || len(all) != 2 {
		t.Fatalf("SettingsOfAll = %+v err=%v, want both tenants", all, err)
	}
	names := map[string]string{}
	for _, entry := range all {
		names[entry.TenantID] = entry.Settings.Channels[0].Name
	}
	if names[tenantA] != "tenant a" || names[tenantB] != "tenant b" {
		t.Fatalf("SettingsOfAll attributed settings to the wrong tenant: %v", names)
	}
}

func TestAlertEventsAreKeptNewestFirstAndBounded(t *testing.T) {
	initAlertTestDB(t)
	store := AlertSettings{}
	base := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	if err := store.AppendEvents(tenantA, []alerting.Event{{ID: "old", At: base}, {ID: "mid", At: base.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	// A retried write must not double an entry.
	if err := store.AppendEvents(tenantA, []alerting.Event{{ID: "mid", At: base.Add(time.Minute)}, {ID: "new", At: base.Add(2 * time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	events, err := store.LoadEvents(tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].ID != "new" || events[1].ID != "mid" || events[2].ID != "old" {
		t.Fatalf("history = %+v, want new, mid, old", events)
	}

	var many []alerting.Event
	for i := 0; i < alerting.MaxEvents+25; i++ {
		many = append(many, alerting.Event{ID: fmt.Sprintf("bulk-%d", i), At: base.Add(time.Hour + time.Duration(i)*time.Second)})
	}
	if err := store.AppendEvents(tenantA, many); err != nil {
		t.Fatal(err)
	}
	events, _ = store.LoadEvents(tenantA)
	if len(events) != alerting.MaxEvents {
		t.Fatalf("history holds %d entries, want it capped at %d", len(events), alerting.MaxEvents)
	}
	if events[0].ID != fmt.Sprintf("bulk-%d", alerting.MaxEvents+24) {
		t.Fatalf("newest entry = %s, want the last appended", events[0].ID)
	}
}

// A row nobody can parse must not lock the page or the engine out: it reads as
// absent, at its real version, so that the next write replaces it.
func TestAlertUnreadableRowsAreReplacedNotFatal(t *testing.T) {
	initAlertTestDB(t)
	store := AlertSettings{}
	db := usage.RuntimeDB()
	for _, key := range []string{alerting.SettingKey, alerting.SentKey, alerting.EventsKey} {
		if _, err := db.Exec(`INSERT INTO runtime_settings (tenant_id, setting_key, payload, version) VALUES (?, ?, ?, 7)`, tenantA, key, `{"not": json at all`); err != nil {
			t.Fatalf("seed a corrupt %s row: %v", key, err)
		}
	}

	if _, version, found, err := store.LoadSettings(tenantA); err != nil || found || version != 7 {
		t.Fatalf("corrupt settings: version=%d found=%v err=%v, want absent at version 7", version, found, err)
	}
	if err := store.SaveSettings(tenantA, sampleSettings("repaired"), 7); err != nil {
		t.Fatalf("a save at the corrupt row's version must replace it: %v", err)
	}
	if got, _, found, _ := store.LoadSettings(tenantA); !found || got.Channels[0].Name != "repaired" {
		t.Fatalf("settings after repair = %+v found=%v", got, found)
	}

	if state, version, err := store.LoadSent(tenantA); err != nil || version != 7 || len(state.Records) != 0 {
		t.Fatalf("corrupt sent table: state=%+v version=%d err=%v, want empty at version 7", state, version, err)
	}

	if _, err := store.LoadEvents(tenantA); err == nil {
		t.Fatal("reading a corrupt history must report it, not pretend it is empty")
	}
	if err := store.AppendEvents(tenantA, []alerting.Event{{ID: "fresh", At: time.Now()}}); err != nil {
		t.Fatalf("appending over a corrupt history must replace it: %v", err)
	}
	if events, err := store.LoadEvents(tenantA); err != nil || len(events) != 1 || events[0].ID != "fresh" {
		t.Fatalf("history after repair = %+v err=%v", events, err)
	}
}

func TestAlertSettingsOfAllSkipsUnreadableRows(t *testing.T) {
	initAlertTestDB(t)
	store := AlertSettings{}
	if err := store.SaveSettings(tenantA, sampleSettings("good"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.RuntimeDB().Exec(`INSERT INTO runtime_settings (tenant_id, setting_key, payload, version) VALUES (?, ?, ?, 1)`, tenantB, alerting.SettingKey, `nope`); err != nil {
		t.Fatal(err)
	}
	all, err := store.SettingsOfAll()
	if err != nil || len(all) != 1 || all[0].TenantID != tenantA {
		t.Fatalf("SettingsOfAll = %+v err=%v, want only the readable tenant", all, err)
	}
}

func TestAlertSettingsAreNotPersistentWithoutADatabase(t *testing.T) {
	usage.CloseDB()
	if (AlertSettings{}).Persistent() {
		t.Fatal("with no database the store must report that nothing is saved")
	}
}
