package usage

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

func TestRedisRuntimeLockAndQueue(t *testing.T) {
	addr := os.Getenv("CLIRELAY_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("CLIRELAY_REDIS_TEST_ADDR is not set")
	}
	InitRedis(config.RedisConfig{Enable: true, Addr: addr})
	t.Cleanup(StopRedis)
	if redisClient == nil {
		t.Fatal("redis client was not initialized")
	}

	ctx := context.Background()
	lockName := "postgres-migration-test-" + time.Now().UTC().Format("150405.000000000")
	const workers = 16
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := AcquireRedisRuntimeLock(ctx, lockName, "owner-"+string(rune('a'+i)), time.Minute)
			if err != nil {
				t.Errorf("AcquireRedisRuntimeLock worker %d: %v", i, err)
				return
			}
			if ok {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("lock winners = %d, want 1", got)
	}
	if released, err := ReleaseRedisRuntimeLock(ctx, lockName, "not-owner"); err != nil || released {
		t.Fatalf("ReleaseRedisRuntimeLock(non-owner) released=%v err=%v, want false nil", released, err)
	}

	queue := "runtime-rebuild-test-" + time.Now().UTC().Format("150405.000000000")
	payload, err := json.Marshal(map[string]string{"table": "api_keys"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	first := RedisRuntimeJob{ID: "job-1", Kind: "cache-rebuild", Payload: payload}
	second := RedisRuntimeJob{ID: "job-2", Kind: "cache-rebuild", Payload: payload}
	if err := EnqueueRedisRuntimeJob(ctx, queue, first); err != nil {
		t.Fatalf("EnqueueRedisRuntimeJob first: %v", err)
	}
	if err := EnqueueRedisRuntimeJob(ctx, queue, second); err != nil {
		t.Fatalf("EnqueueRedisRuntimeJob second: %v", err)
	}
	gotFirst, err := DequeueRedisRuntimeJob(ctx, queue, time.Second)
	if err != nil {
		t.Fatalf("DequeueRedisRuntimeJob first: %v", err)
	}
	gotSecond, err := DequeueRedisRuntimeJob(ctx, queue, time.Second)
	if err != nil {
		t.Fatalf("DequeueRedisRuntimeJob second: %v", err)
	}
	if gotFirst == nil || gotSecond == nil || gotFirst.ID != "job-1" || gotSecond.ID != "job-2" {
		t.Fatalf("queue order first=%#v second=%#v, want FIFO job-1/job-2", gotFirst, gotSecond)
	}
}

func TestRedisUnavailableDoesNotBlockPostgresCRUD(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	db := getDB()
	if db == nil {
		t.Fatal("postgres db is nil")
	}
	if _, err := db.Exec(`TRUNCATE api_keys RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate api_keys: %v", err)
	}

	InitRedis(config.RedisConfig{Enable: true, Addr: "127.0.0.1:1"})
	if redisClient != nil {
		t.Fatal("redis client should stay nil when Redis is unavailable")
	}
	if err := UpsertAPIKey(APIKeyRow{Key: "sk-redis-fallback", ID: "redis-fallback", Name: "Redis Fallback"}); err != nil {
		t.Fatalf("UpsertAPIKey with redis unavailable: %v", err)
	}
	if got := GetAPIKeyByID("redis-fallback"); got == nil || got.Key != "sk-redis-fallback" {
		t.Fatalf("GetAPIKeyByID with redis unavailable = %#v", got)
	}
}
