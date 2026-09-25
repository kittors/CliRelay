package compatdriver

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseConnConfigDefaultsConnectTimeout(t *testing.T) {
	unsetEnv(t, "PGCONNECT_TIMEOUT")
	cases := map[string]time.Duration{
		"postgres://u:p@127.0.0.1:5432/db?sslmode=disable":                    DefaultConnectTimeout,
		"host=127.0.0.1 user=u dbname=db sslmode=disable":                     DefaultConnectTimeout,
		"postgres://u:p@127.0.0.1:5432/db?sslmode=disable&connect_timeout=12": 12 * time.Second,
		// An explicit 0 is the operator's choice and is kept.
		"host=127.0.0.1 user=u dbname=db sslmode=disable connect_timeout=0": 0,
	}
	for dsn, want := range cases {
		cfg, err := ParseConnConfig(dsn)
		if err != nil {
			t.Fatalf("ParseConnConfig(%q): %v", dsn, err)
		}
		if cfg.ConnectTimeout != want {
			t.Errorf("ParseConnConfig(%q).ConnectTimeout = %s, want %s", dsn, cfg.ConnectTimeout, want)
		}
	}
}

func TestParseConnConfigHonoursConnectTimeoutEnv(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "7")
	cfg, err := ParseConnConfig("postgres://u:p@127.0.0.1:5432/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 7*time.Second {
		t.Fatalf("ConnectTimeout = %s, want the PGCONNECT_TIMEOUT value", cfg.ConnectTimeout)
	}
}

// A Patroni deployment lists every member and lets libpq-style
// target_session_attrs pick the primary; parsing must keep both.
func TestParseConnConfigKeepsMultiHostFailoverDSN(t *testing.T) {
	unsetEnv(t, "PGCONNECT_TIMEOUT")
	cfg, err := ParseConnConfig("postgres://u:p@10.0.0.1:5432,10.0.0.2:5433/db?sslmode=disable&target_session_attrs=read-write")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "10.0.0.1" || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].Host != "10.0.0.2" || cfg.Fallbacks[0].Port != 5433 {
		t.Fatalf("hosts not preserved: primary %s fallbacks %+v", cfg.Host, cfg.Fallbacks)
	}
	if cfg.ValidateConnect == nil {
		t.Fatal("target_session_attrs=read-write must install the read-write check")
	}
	if cfg.ConnectTimeout != DefaultConnectTimeout {
		t.Fatalf("ConnectTimeout = %s", cfg.ConnectTimeout)
	}
}

func TestKeepAliveDialerFollowsProcessPolicy(t *testing.T) {
	t.Cleanup(func() { SetDialKeepAlive(net.KeepAliveConfig{}) })
	if keepAliveDialer(time.Second) != nil {
		t.Fatal("no keepalive policy must keep pgx's default dialer")
	}
	policy := net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}
	SetDialKeepAlive(policy)
	dialer := keepAliveDialer(3 * time.Second)
	if dialer == nil || dialer.KeepAliveConfig != policy || dialer.Timeout != 3*time.Second {
		t.Fatalf("dialer = %+v, want keepalive %+v and timeout 3s", dialer, policy)
	}
	SetDialKeepAlive(net.KeepAliveConfig{})
	if keepAliveDialer(time.Second) != nil {
		t.Fatal("disabling the policy must restore the default dialer")
	}
}

func TestOpenKeepsDriverTypeName(t *testing.T) {
	db, err := sql.Open(DriverName, "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// apikey's SQLite detection matches on this type name.
	if got := fmt.Sprintf("%T", db.Driver()); !strings.Contains(got, "compatdriver") {
		t.Fatalf("db.Driver() type = %s", got)
	}
}

func TestOpenRejectsMalformedDSN(t *testing.T) {
	if _, err := sql.Open(DriverName, "postgres://u:p@[::1/db"); err == nil {
		t.Fatal("malformed DSN must fail at open")
	}
}

// unsetEnv removes key for the test; t.Setenv registers the restore.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}
