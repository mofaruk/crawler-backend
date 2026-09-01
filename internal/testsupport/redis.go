// Package testsupport holds helpers shared by tests across packages.
package testsupport

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultRedisAddr is the port docker-compose publishes for local development.
const DefaultRedisAddr = "localhost:6382"

// RequireRedisEnv makes a missing Redis a failure rather than a skip.
//
// Set it in CI. Without it these tests skip silently, which means the queue,
// the rate limiter and the adaptive speed controller can all break while the
// suite stays green — the failure mode this exists to prevent.
const RequireRedisEnv = "REQUIRE_REDIS"

// RedisAddr is the address tests connect to, overridable for a CI service
// container on a different host or port.
func RedisAddr() string {
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		return addr
	}

	return DefaultRedisAddr
}

// Redis returns a client for the test Redis, skipping the test when it is
// unreachable — unless REQUIRE_REDIS is set, in which case it fails.
//
// Skipping is right locally: the suite should stay runnable without starting
// infrastructure. It is wrong in CI, where a skipped test looks identical to a
// passing one and nothing reports that a package went untested.
func Redis(t *testing.T) *redis.Client {
	t.Helper()

	addr := RedisAddr()
	rdb := redis.NewClient(&redis.Options{Addr: addr})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		if os.Getenv(RequireRedisEnv) != "" {
			t.Fatalf("redis is required (%s is set) but unreachable at %s: %v",
				RequireRedisEnv, addr, err)
		}

		t.Skipf("redis unavailable at %s: %v — set %s to make this a failure",
			addr, err, RequireRedisEnv)
	}

	t.Cleanup(func() { _ = rdb.Close() })

	return rdb
}
