package usage

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresMigrationTimeoutDefault(t *testing.T) {
	unsetEnv(t, postgresMigrationTimeoutEnv)

	got, err := postgresMigrationTimeout()
	if err != nil {
		t.Fatalf("postgresMigrationTimeout() error = %v", err)
	}
	if got != 30*time.Second {
		t.Fatalf("postgresMigrationTimeout() = %s, want 30s", got)
	}
}

func TestPostgresMigrationTimeoutFromEnv(t *testing.T) {
	t.Setenv(postgresMigrationTimeoutEnv, " 5m ")

	got, err := postgresMigrationTimeout()
	if err != nil {
		t.Fatalf("postgresMigrationTimeout() error = %v", err)
	}
	if got != 5*time.Minute {
		t.Fatalf("postgresMigrationTimeout() = %s, want 5m", got)
	}
}

func TestPostgresMigrationTimeoutRejectsInvalidValue(t *testing.T) {
	for _, value := range []string{"not-a-duration", "0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(postgresMigrationTimeoutEnv, value)

			_, err := postgresMigrationTimeout()
			if err == nil {
				t.Fatal("postgresMigrationTimeout() error = nil, want invalid value error")
			}
			if !strings.Contains(err.Error(), postgresMigrationTimeoutEnv) {
				t.Fatalf("postgresMigrationTimeout() error = %q, want env name", err)
			}
		})
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	value, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			if err := os.Setenv(key, value); err != nil {
				t.Errorf("restore %s: %v", key, err)
			}
			return
		}
		if err := os.Unsetenv(key); err != nil {
			t.Errorf("clear %s: %v", key, err)
		}
	})
}
