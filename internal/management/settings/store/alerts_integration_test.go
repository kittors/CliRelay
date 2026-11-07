package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// These tests run the real engine on the real settings adapter, on SQLite and,
// when CLIRELAY_POSTGRES_TEST_DSN is set, on PostgreSQL. The property they
// protect cannot be seen on an in-memory store: during a deploy two backend
// slots evaluate at the same time against one database, and a problem must still
// reach the channel once.

// dialect tells a test which database it runs on, for the few statements that
// are written by hand.
type dialect struct{ postgres bool }

// backends runs fn against each database the environment offers.
func backends(t *testing.T, fn func(t *testing.T, db dialect)) {
	t.Run("sqlite", func(t *testing.T) {
		initAlertTestDB(t)
		fn(t, dialect{})
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
		if dsn == "" {
			t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
		}
		postgrestest.LockSharedRuntimeDB(t, dsn)
		usage.CloseDB()
		t.Cleanup(usage.CloseDB)
		if err := usage.InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
			t.Fatalf("InitPostgres: %v", err)
		}
		fn(t, dialect{postgres: true})
	})
}

// addTenant creates a tenant row on PostgreSQL, which enforces the foreign key
// from the settings rows to tenants, and removes everything stored for it
// afterwards so a rerun on a shared database starts clean. The SQLite database
// of these tests has no tenants table and is thrown away with the test.
func (d dialect) addTenant(t *testing.T, tenantID string) {
	t.Helper()
	if !d.postgres {
		return
	}
	db := usage.RuntimeDB()
	slug := "alerts-" + tenantID[:8]
	// A standard tenant must carry an expiry (check constraint tenants_standard_expiry).
	if _, err := db.Exec(`INSERT INTO tenants (id, slug, name, type, status, expires_at)
		VALUES ($1, $2, $2, 'standard', 'active', now() + interval '1 day')`, tenantID, slug); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"config_versions", "runtime_settings"} {
			_, _ = db.Exec(`DELETE FROM `+table+` WHERE tenant_id = $1`, tenantID)
		}
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
}

// fixedSources reports one account that has used 95% of its weekly window.
type fixedSources struct{ now time.Time }

func (s fixedSources) Accounts(context.Context, string, time.Time) ([]alerting.AccountSample, error) {
	return []alerting.AccountSample{{
		ID: "codex-1", Subject: "subject-1", Provider: "codex", Name: "main",
		Windows: []alerting.WindowSample{{
			Key: "code_week", Label: "Weekly", Span: 7 * 24 * time.Hour, UsedPercent: 95,
			ResetAt: s.now.Add(48 * time.Hour), ObservedAt: s.now.Add(-5 * time.Minute), AccountWide: true,
		}},
	}}, nil
}

func (fixedSources) Spenders(context.Context, string, time.Time) ([]alerting.SpendSample, error) {
	return nil, nil
}

// countingSender counts the messages it is asked to deliver.
type countingSender struct {
	mu       sync.Mutex
	messages []alerting.Message
	delay    time.Duration
}

func (s *countingSender) Send(_ context.Context, channels []alerting.Channel, msg alerting.Message, _ bool) []alerting.Delivery {
	if s.delay > 0 {
		time.Sleep(s.delay) // a slow receiver: the window in which the other slot also looks
	}
	s.mu.Lock()
	s.messages = append(s.messages, msg)
	s.mu.Unlock()
	out := make([]alerting.Delivery, 0, len(channels))
	for _, c := range channels {
		out = append(out, alerting.Delivery{ChannelID: c.ID, ChannelName: c.Name, Kind: c.Kind, Status: alerting.DeliverySent})
	}
	return out
}

func (s *countingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

type testClock struct{ now atomic.Pointer[time.Time] }

func newTestClock(t time.Time) *testClock {
	c := &testClock{}
	c.now.Store(&t)
	return c
}
func (c *testClock) Now() time.Time          { return *c.now.Load() }
func (c *testClock) Advance(d time.Duration) { t := c.Now().Add(d); c.now.Store(&t) }

func (d dialect) armTenant(t *testing.T, tenantID string) {
	t.Helper()
	d.addTenant(t, tenantID)
	settings := alerting.DefaultSettings()
	settings.Enabled = true
	settings.Channels = []alerting.Channel{{ID: "ch-1", Name: "ops", Kind: alerting.KindWebhook, Enabled: true, URL: "https://hooks.example.com/alerts"}}
	if err := (AlertSettings{}).SaveSettings(tenantID, settings, 0); err != nil {
		t.Fatalf("arm the tenant: %v", err)
	}
}

func TestEngineOnTheRealStoreSendsOnceAndRemembersIt(t *testing.T) {
	backends(t, func(t *testing.T, db dialect) {
		tenant := uuid.NewString()
		db.armTenant(t, tenant)
		clock := newTestClock(time.Now().UTC().Truncate(time.Second))
		sender := &countingSender{}
		engine := alerting.NewEngine(alerting.EngineOptions{
			Store: AlertSettings{}, Sources: fixedSources{now: clock.Now()}, Sender: sender, Now: clock.Now,
		})

		engine.RunOnce(context.Background())
		if sender.count() != 1 {
			t.Fatalf("delivered %d messages, want 1 for the one problem", sender.count())
		}
		msg := sender.messages[0]
		if msg.Rule != alerting.RuleQuotaWindow || msg.TenantID != tenant || msg.Subject.Kind != alerting.SubjectAccount {
			t.Fatalf("message = %+v", msg)
		}

		// The next rounds find the same problem and stay quiet: what was sent is
		// remembered in the database, not in this process.
		clock.Advance(2 * time.Minute)
		engine.RunOnce(context.Background())
		clock.Advance(20 * time.Minute)
		fresh := alerting.NewEngine(alerting.EngineOptions{
			Store: AlertSettings{}, Sources: fixedSources{now: clock.Now()}, Sender: sender, Now: clock.Now,
		})
		fresh.RunOnce(context.Background()) // a restarted process knows nothing but the database
		if sender.count() != 1 {
			t.Fatalf("delivered %d messages, want the problem reported once", sender.count())
		}

		events, err := (AlertSettings{}).LoadEvents(tenant)
		if err != nil || len(events) != 1 || events[0].Rule != alerting.RuleQuotaWindow {
			t.Fatalf("history = %+v err=%v, want the one message", events, err)
		}
		if len(events[0].Deliveries) != 1 || events[0].Deliveries[0].Status != alerting.DeliverySent {
			t.Fatalf("deliveries = %+v", events[0].Deliveries)
		}
		state, _, _ := (AlertSettings{}).LoadSent(tenant)
		if len(state.Records) != 1 {
			t.Fatalf("sent table = %+v, want one record", state.Records)
		}
		for key, record := range state.Records {
			if record.Pending || record.Failed {
				t.Fatalf("record %s = %+v, want the reservation settled into a quiet period", key, record)
			}
			if !record.Until.After(clock.Now()) {
				t.Fatalf("record %s is quiet until %v, want a time in the future", key, record.Until)
			}
		}

		// The window renews and fills up again: a new cycle, a new message.
		clock.Advance(49 * time.Hour)
		again := alerting.NewEngine(alerting.EngineOptions{
			Store: AlertSettings{}, Sources: fixedSources{now: clock.Now()}, Sender: sender, Now: clock.Now,
		})
		again.RunOnce(context.Background())
		if sender.count() != 2 {
			t.Fatalf("delivered %d messages after the window renewed, want 2", sender.count())
		}
	})
}

// The two backend slots of a deploy evaluate at once. Whichever reserves the
// problem first sends it; the other must find the reservation and stay quiet.
func TestTwoEnginesOnOneDatabaseSendEachProblemOnce(t *testing.T) {
	backends(t, func(t *testing.T, db dialect) {
		for round := 0; round < 12; round++ {
			tenant := uuid.NewString()
			db.armTenant(t, tenant)
			now := time.Now().UTC().Truncate(time.Second)
			// A slow receiver keeps the first sender busy while the second looks.
			sender := &countingSender{delay: 40 * time.Millisecond}
			newEngine := func() *alerting.Engine {
				return alerting.NewEngine(alerting.EngineOptions{
					Store: AlertSettings{}, Sources: fixedSources{now: now}, Sender: sender, Now: func() time.Time { return now },
				})
			}
			slotA, slotB := newEngine(), newEngine()

			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, engine := range []*alerting.Engine{slotA, slotB} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					engine.RunOnce(context.Background())
				}()
			}
			close(start)
			wg.Wait()

			if sender.count() != 1 {
				t.Fatalf("round %d: the problem was delivered %d times by two engines, want exactly once", round, sender.count())
			}
			events, _ := (AlertSettings{}).LoadEvents(tenant)
			if len(events) != 1 {
				t.Fatalf("round %d: history holds %d entries, want 1", round, len(events))
			}
		}
	})
}

func TestEngineOnTheRealStoreIgnoresTenantsThatSwitchedAlertingOff(t *testing.T) {
	backends(t, func(t *testing.T, db dialect) {
		off, on := uuid.NewString(), uuid.NewString()
		db.armTenant(t, on)
		db.addTenant(t, off)
		settings := alerting.DefaultSettings()
		settings.Enabled = false
		settings.Channels = []alerting.Channel{{ID: "ch-1", Name: "ops", Kind: alerting.KindWebhook, Enabled: true, URL: "https://hooks.example.com/alerts"}}
		if err := (AlertSettings{}).SaveSettings(off, settings, 0); err != nil {
			t.Fatal(err)
		}
		sender := &countingSender{}
		engine := alerting.NewEngine(alerting.EngineOptions{Store: AlertSettings{}, Sources: fixedSources{now: time.Now()}, Sender: sender})
		engine.RunOnce(context.Background())
		// A shared database may hold rows of other tests' tenants; only these two matter.
		forOn, forOff := 0, 0
		for _, m := range sender.messages {
			switch m.TenantID {
			case on:
				forOn++
			case off:
				forOff++
			}
		}
		if forOn != 1 || forOff != 0 {
			t.Fatalf("delivered %d messages for the tenant with alerting on and %d for the one with it off, want 1 and 0", forOn, forOff)
		}
		if state, _, _ := (AlertSettings{}).LoadSent(off); len(state.Records) != 0 {
			t.Fatalf("a tenant with alerting off has a sent table: %+v", state)
		}
	})
}
