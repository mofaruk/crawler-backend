# Distributed Crawler Backend - Architecture Document

## 1. High-Level Architecture

```
                                    ┌─────────────────────────────────────────────────┐
                                    │              Observability Stack                 │
                                    │  ┌─────────────┐      ┌────────────┐            │
                                    │  │ Prometheus   │─────▶│  Grafana   │            │
                                    │  └──────┬──────┘      └────────────┘            │
                                    │         │ scrape /metrics                        │
                                    └─────────┼───────────────────────────────────────┘
                                              │
┌──────────┐    HTTP     ┌────────────────────┼──────────────────────────────────┐
│  Client   │───────────▶│                    │        Crawler Backend           │
└──────────┘             │  ┌─────────────────┴──────────┐                      │
                         │  │         API Service         │                      │
                         │  │  • Site CRUD                │                      │
                         │  │  • Crawling lifecycle       │                      │
                         │  │  • URL ingestion (async)    │                      │
                         │  │  • Progress reporting       │                      │
                         │  └─────────┬──────────────────┘                      │
                         │            │                                          │
                         │            ▼                                          │
                         │  ┌──────────────────────────────────────────────┐     │
                         │  │                   Redis                      │     │
                         │  │  • Distributed Queue (LIST + ZSET)          │     │
                         │  │  • Rate Limiter (Token Bucket + Lua)        │     │
                         │  │  • Deduplication (SET of URL hashes)        │     │
                         │  │  • Job State (STRING per crawling)          │     │
                         │  │  • Active Crawlings Registry (SET)          │     │
                         │  └─────────┬──────────────────────────────────┘     │
                         │            │                                          │
                         │            ▼                                          │
                         │  ┌─────────────────────────────────────────────┐      │
                         │  │            Worker Cluster                    │      │
                         │  │                                             │      │
                         │  │  ┌─────────┐ ┌─────────┐    ┌─────────┐   │      │
                         │  │  │Worker 1 │ │Worker 2 │ ···│Worker N │   │      │
                         │  │  │200 grt  │ │200 grt  │    │200 grt  │   │      │
                         │  │  └────┬────┘ └────┬────┘    └────┬────┘   │      │
                         │  │       │           │              │         │      │
                         │  └───────┼───────────┼──────────────┼────────┘      │
                         │          │           │              │                │
                         │          ▼           ▼              ▼                │
                         │  ┌──────────────────────────────────────────────┐    │
                         │  │                MongoDB                        │    │
                         │  │  • sites          • crawling_results         │    │
                         │  │  • crawlings      • crawl_failures           │    │
                         │  │  • crawl_urls                                │    │
                         │  └──────────────────────────────────────────────┘    │
                         └──────────────────────────────────────────────────────┘
```

### Component Responsibilities

| Component | Responsibility |
|-----------|---------------|
| **API Service** | HTTP endpoints, site management, crawl lifecycle, URL ingestion, progress queries |
| **Worker** | URL fetching, rate limit enforcement, result storage, retry handling, job state checking |
| **Redis** | Distributed queue, rate limiter state, deduplication sets, job state propagation |
| **MongoDB** | Persistent storage for sites, crawlings, results, failures |
| **Prometheus** | Metrics collection from API and all worker instances |
| **Grafana** | Dashboards for crawl rate, latency, queue depth, errors |

---

## 2. Distributed Queue Design

### Architecture

The queue uses Redis data structures for atomic, distributed task management:

```
┌──────────────────────────────────────────────────────────┐
│                    Per-Crawling Queue                      │
│                                                           │
│  PENDING (LIST)     PROCESSING (ZSET)    RETRY (ZSET)    │
│  ┌─────────────┐   ┌──────────────┐    ┌─────────────┐  │
│  │ task_json    │   │ task : ts    │    │ task : ts   │  │
│  │ task_json    │──▶│ task : ts    │──▶ │ task : ts   │  │
│  │ task_json    │   │ task : ts    │    │ task : ts   │  │
│  └─────────────┘   └──────────────┘    └─────────────┘  │
│                                               │           │
│                     DEAD (LIST)               │           │
│                    ┌─────────────┐  ◀─────────┘           │
│                    │ task_json   │  (max retries)         │
│                    └─────────────┘                        │
└──────────────────────────────────────────────────────────┘
```

### Redis Keys

| Key Pattern | Type | Purpose |
|------------|------|---------|
| `crawl:{id}:pending` | LIST | URLs waiting to be crawled |
| `crawl:{id}:processing` | ZSET | URLs being crawled (score = unix timestamp) |
| `crawl:{id}:retry` | ZSET | Failed URLs awaiting retry (score = retry-at timestamp) |
| `crawl:{id}:dead` | LIST | Permanently failed URLs (exceeded max retries) |

### Consumption Strategy

1. **Atomic dequeue**: Lua script `RPOP` from pending + `ZADD` to processing in one atomic operation
2. **Batch dequeue**: Workers can dequeue up to N tasks at once for throughput
3. **Acknowledgment**: On success, `ZREM` from processing
4. **Retry**: On failure, `ZREM` from processing + `ZADD` to retry with exponential backoff score
5. **Recovery**: Background goroutine scans processing ZSET for stale entries (>5 min) and requeues

### Task Format (JSON)

```json
{
  "crawling_id": "65f1a2b3...",
  "site_id": "65f1a2b0...",
  "url": "https://example.com/page",
  "url_hash": "a1b2c3d4e5f6...",
  "user_agent": "WK-Crawler/1.0",
  "extract_data": ["Content-Type", "X-Custom-Header"],
  "retries": 0,
  "max_retries": 3,
  "enqueued_at": 1710000000
}
```

---

## 3. Distributed Deduplication

### Architecture

```
                URL                     SHA-256 (128-bit)
"https://example.com/page"  ──────▶  "a1b2c3d4e5f6a7b8..."
                                            │
                                            ▼
                                  Redis SET: crawl:{id}:seen
                                  ┌──────────────────────┐
                                  │ SADD returns 1 (new) │───▶ enqueue
                                  │ SADD returns 0 (dup) │───▶ skip
                                  └──────────────────────┘
```

| Redis Key | Type | Purpose |
|----------|------|---------|
| `crawl:{id}:seen` | SET | Set of URL hashes for deduplication |

### Design Decisions

- **SHA-256 truncated to 128 bits**: 16 bytes per URL hash. Collision probability negligible for billions of URLs
- **Redis SET over Bloom filter**: Zero false positives. For 10M URLs, ~160MB memory
- **Batch marking**: Lua script marks multiple hashes atomically, returns count of newly added
- For 100M+ URL jobs, swap to Redis Bloom filter module (`BF.ADD`) to reduce memory 10x

---

## 4. Crawl Rate Limiting

### Distributed Token Bucket

Rate limits are **global across all worker containers** using a Redis-backed token bucket.

```
                       ┌─────────────────────────────────┐
                       │     Redis Token Bucket           │
                       │                                  │
  Worker 1 ──Acquire──▶│  tokens_key  = 15.7 (float)    │
  Worker 2 ──Acquire──▶│  refill_key  = 1710000123456   │
  Worker 3 ──Acquire──▶│  speed_key   = 36000           │
                       │                                  │
                       │  Lua script (atomic):            │
                       │  1. elapsed = now - last_refill  │
                       │  2. new_tokens = elapsed * rate  │
                       │  3. tokens = min(t + new, burst) │
                       │  4. consume requested tokens     │
                       └─────────────────────────────────┘
```

| Redis Key | Type | Purpose |
|----------|------|---------|
| `crawl:{id}:tokens` | STRING | Current token count (float) |
| `crawl:{id}:tokens:refill` | STRING | Last refill timestamp (milliseconds) |
| `crawl:{id}:speed` | STRING | Configured speed (URLs/hour) |

### Lua Script Behavior

```lua
-- Atomic token bucket refill + consume
rate_per_ms = speed / 3600000
new_tokens = elapsed_ms * rate_per_ms
burst = max(ceil(speed / 3600) * 2, 10)
tokens = min(tokens + new_tokens, burst)

if tokens >= requested then
    return requested  -- grant all
elseif tokens >= 1 then
    return floor(tokens)  -- grant partial
else
    return 0  -- rate limited
end
```

### Example

- Speed = 36000 URLs/hour = 10 URLs/second
- Burst capacity = 20 tokens
- Worker requests 1 token → granted if available, else waits and retries on next poll

---

## 5. Crawl Politeness

### Per-Domain Concurrency Limiting

```go
type DomainLimiter struct {
    limiters map[string]chan struct{}  // buffered channel per domain
    maxConc  int                       // e.g., 10 concurrent per domain
    delay    time.Duration             // e.g., 100ms between requests
}
```

- **Max 10 concurrent requests per domain** across the worker container
- **100ms minimum delay** between requests to the same domain
- Domains are extracted from URLs and used as map keys

### robots.txt Handling

The current implementation focuses on user-agent and request delay. For full robots.txt compliance, add:
- Cache `robots.txt` per domain in Redis with TTL
- Parse disallow/allow rules before enqueueing URLs
- Respect `Crawl-delay` directive

---

## 6. Retry Strategy

```
Failed URL
    │
    ▼
┌──────────┐     retries < max?    ┌────────────────┐
│ Remove   │────── YES ──────────▶│ Add to RETRY   │
│ from     │                       │ ZSET with score│
│ PROCESSING│                      │ = now + backoff│
└──────────┘                       └───────┬────────┘
    │                                      │
    │ NO                                   │ Recovery loop
    ▼                                      │ checks every 30s
┌──────────┐                               ▼
│ Add to   │                       ┌────────────────┐
│ DEAD     │                       │ Move to PENDING│
│ letter Q │                       │ when score ≤   │
└──────────┘                       │ current time   │
                                   └────────────────┘
```

### Exponential Backoff Schedule

| Retry | Backoff |
|-------|---------|
| 1 | 2 seconds |
| 2 | 4 seconds |
| 3 | 8 seconds |
| 4+ | 300 seconds (cap) |

### Retry-Worthy Failures

- Network errors (timeout, connection refused)
- HTTP 5xx status codes
- DNS resolution failures

### Non-Retryable

- HTTP 4xx status codes → stored as failures immediately
- Invalid URL format

---

## 7. Worker Architecture

### Lifecycle

```
┌──────────────────────────────────────────────────┐
│                 Worker Container                  │
│                                                   │
│  ┌─────────────────────────────────────────────┐ │
│  │           Worker Pool (N goroutines)         │ │
│  │                                              │ │
│  │  ┌──────┐ ┌──────┐ ┌──────┐    ┌──────┐   │ │
│  │  │ G-1  │ │ G-2  │ │ G-3  │ ···│ G-N  │   │ │
│  │  └──┬───┘ └──┬───┘ └──┬───┘    └──┬───┘   │ │
│  │     │        │        │           │        │ │
│  │     ▼        ▼        ▼           ▼        │ │
│  │  ┌──────────────────────────────────────┐  │ │
│  │  │         Worker Loop (per goroutine)  │  │ │
│  │  │                                      │  │ │
│  │  │  1. Get active crawling IDs          │  │ │
│  │  │  2. Round-robin across jobs          │  │ │
│  │  │  3. Check job state (running?)       │  │ │
│  │  │  4. Acquire rate limit token         │  │ │
│  │  │  5. Dequeue task from Redis          │  │ │
│  │  │  6. Fetch URL (HTTP GET)             │  │ │
│  │  │  7. Store result in MongoDB          │  │ │
│  │  │  8. ACK task / retry on failure      │  │ │
│  │  │  9. Update progress counters         │  │ │
│  │  └──────────────────────────────────────┘  │ │
│  └─────────────────────────────────────────────┘ │
│                                                   │
│  ┌─────────────────────────────────────────────┐ │
│  │         Recovery Goroutine (1)              │ │
│  │  • Requeue stale processing tasks (>5min)  │ │
│  │  • Move ready retries to pending           │ │
│  │  • Update queue metrics                    │ │
│  │  • Runs every 30 seconds                   │ │
│  └─────────────────────────────────────────────┘ │
│                                                   │
│  ┌─────────────────────────────────────────────┐ │
│  │         Metrics Server (:9090)              │ │
│  │  • Prometheus /metrics endpoint             │ │
│  └─────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────┘
```

### Worker Pseudocode

```go
func workerLoop(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        crawlingIDs := stateManager.GetActiveCrawlings()

        for _, id := range crawlingIDs {
            state := stateManager.GetState(id)
            if state != "running" { continue }

            tokens := rateLimiter.Acquire(id, 1)
            if tokens == 0 { continue }

            task := queue.Dequeue(id)
            if task == nil {
                checkJobCompletion(id)
                continue
            }

            result := fetcher.Fetch(ctx, task)

            if result.Error != nil || result.StatusCode >= 500 {
                queue.Retry(id, task)
            } else {
                repo.InsertResult(result)
                queue.Ack(id, task)
            }

            repo.UpdateProgress(id)
        }

        time.Sleep(pollInterval)
    }
}
```

### Key Characteristics

- **Multi-job fairness**: Round-robin across all active crawling jobs
- **Near-real-time state**: Checks Redis state before every fetch
- **Graceful shutdown**: Context cancellation propagates, in-flight tasks requeued
- **Configurable**: `WORKER_CONCURRENCY=200` sets goroutine count

---

## 8. MongoDB Schema Design

### Collection: `sites`

```json
{
  "_id": ObjectId("65f1a2b0..."),
  "name": "Example Blog",
  "base_url": "https://example.com",
  "url_limit": 50000,
  "url_source": "https://example.com/sitemap.xml",
  "url_source_type": "xml",
  "user_agent": "WK-Crawler/1.0",
  "extract_data": ["Content-Type", "X-Powered-By", "Server"],
  "created_at": ISODate("2024-03-09T10:00:00Z"),
  "updated_at": ISODate("2024-03-09T10:00:00Z")
}
```

**Indexes:**
- `{ base_url: 1 }` — unique
- `{ name: 1 }`

### Collection: `crawlings`

```json
{
  "_id": ObjectId("65f1a2b3..."),
  "site_id": ObjectId("65f1a2b0..."),
  "status": "running",
  "speed": 36000,
  "reload_source": false,
  "total_urls": 48532,
  "crawled_urls": 12450,
  "failed_urls": 23,
  "started_at": ISODate("2024-03-09T10:05:00Z"),
  "completed_at": null,
  "paused_at": null,
  "created_at": ISODate("2024-03-09T10:04:55Z"),
  "updated_at": ISODate("2024-03-09T10:30:00Z"),
  "error_message": ""
}
```

**Indexes:**
- `{ site_id: 1, status: 1 }`
- `{ status: 1 }`
- `{ created_at: -1 }`

### Collection: `crawl_urls`

```json
{
  "_id": ObjectId("65f1a300..."),
  "crawling_id": ObjectId("65f1a2b3..."),
  "site_id": ObjectId("65f1a2b0..."),
  "url": "https://example.com/page-1",
  "url_hash": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
  "status": "completed",
  "retries": 0,
  "created_at": ISODate("2024-03-09T10:05:01Z"),
  "updated_at": ISODate("2024-03-09T10:06:00Z")
}
```

**Indexes:**
- `{ crawling_id: 1, status: 1 }`
- `{ crawling_id: 1, url_hash: 1 }` — unique (dedup at storage level)
- `{ url_hash: 1 }`

### Collection: `crawling_results`

```json
{
  "_id": ObjectId("65f1a400..."),
  "crawling_id": ObjectId("65f1a2b3..."),
  "site_id": ObjectId("65f1a2b0..."),
  "url": "https://example.com/page-1",
  "status_code": 200,
  "headers": {
    "Content-Type": "text/html; charset=utf-8",
    "Server": "nginx/1.24.0",
    "X-Powered-By": "Express"
  },
  "body_data": {},
  "content_type": "text/html; charset=utf-8",
  "response_time_ms": 234,
  "crawled_at": ISODate("2024-03-09T10:06:00Z")
}
```

**Indexes:**
- `{ crawling_id: 1 }`
- `{ site_id: 1 }`
- `{ crawled_at: -1 }`

### Collection: `crawl_failures`

```json
{
  "_id": ObjectId("65f1a500..."),
  "crawling_id": ObjectId("65f1a2b3..."),
  "site_id": ObjectId("65f1a2b0..."),
  "url": "https://example.com/broken-page",
  "error": "connection timeout after 30s",
  "status_code": 0,
  "retries": 3,
  "failed_at": ISODate("2024-03-09T10:10:00Z")
}
```

**Indexes:**
- `{ crawling_id: 1 }`
- `{ failed_at: -1 }`

### Sharding Strategy (for production at scale)

| Collection | Shard Key | Rationale |
|-----------|-----------|-----------|
| `crawling_results` | `{ crawling_id: "hashed" }` | Even distribution, queries are per-crawling |
| `crawl_urls` | `{ crawling_id: 1, url_hash: 1 }` | Compound key for locality + uniqueness |
| `crawl_failures` | `{ crawling_id: "hashed" }` | Low volume, even distribution |

---

## 9. Redis Key Design (Complete Reference)

| Key | Type | TTL | Purpose |
|-----|------|-----|---------|
| `crawl:{id}:pending` | LIST | None | Pending URLs to crawl |
| `crawl:{id}:processing` | ZSET | None | URLs being processed (score = enqueue time) |
| `crawl:{id}:retry` | ZSET | None | Failed URLs awaiting retry (score = retry-at) |
| `crawl:{id}:dead` | LIST | None | Dead letter queue |
| `crawl:{id}:seen` | SET | None | URL hash deduplication set |
| `crawl:{id}:state` | STRING | None | Job state (running/paused/stopped) |
| `crawl:{id}:tokens` | STRING | None | Rate limiter token count |
| `crawl:{id}:tokens:refill` | STRING | None | Last token refill timestamp (ms) |
| `crawl:{id}:speed` | STRING | None | Configured crawl speed (URLs/hour) |
| `active:crawlings` | SET | None | Registry of currently active crawling IDs |

### Memory Estimation

For a crawl job with 1M URLs:
- Pending queue: ~200MB (200 bytes/task avg)
- Dedup set: ~16MB (16 bytes/hash)
- Processing set: ~2MB (max ~10K concurrent)
- Total: ~220MB per 1M-URL job

---

## 10. Horizontal Scaling Strategy

### Scaling Workers

```bash
# Scale to 20 worker containers, each with 200 goroutines = 4,000 concurrent workers
docker compose up --scale worker=20
```

### How Scaling Works

```
                    ┌─────────────────────────┐
                    │         Redis            │
                    │   (single source of      │
                    │    truth for queues)      │
                    └────────┬────────────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
         ┌────┴────┐   ┌────┴────┐   ┌────┴────┐
         │Worker 1 │   │Worker 2 │   │Worker N │
         │200 grt  │   │200 grt  │   │200 grt  │
         └─────────┘   └─────────┘   └─────────┘

  Total throughput = N × WORKER_CONCURRENCY × (URLs processed per goroutine)
```

**No coordination needed between workers** because:
1. Redis queues provide atomic dequeue (no double-processing)
2. Rate limiter is global in Redis (all workers share the same bucket)
3. Job state is in Redis (all workers see pause/stop immediately)
4. Dedup set is in Redis (all workers check same set)

### Throughput Calculation

| Workers | Goroutines | Speed (avg 200ms/req) | Actual Rate |
|---------|------------|----------------------|-------------|
| 1 | 200 | 1,000 URLs/sec | ~3.6M/hour |
| 5 | 1,000 | 5,000 URLs/sec | ~18M/hour |
| 10 | 2,000 | 10,000 URLs/sec | ~36M/hour |
| 20 | 4,000 | 20,000 URLs/sec | ~72M/hour |

Note: Actual throughput is bounded by: rate limiter setting, network bandwidth, target site response times.

---

## 11. Fault Tolerance

### Worker Crash Recovery

```
Worker crashes
    │
    ▼
Tasks in PROCESSING ZSET remain with timestamp
    │
    ▼
Recovery loop (other workers) detects stale tasks (>5 min)
    │
    ▼
Requeues to PENDING ── no URL loss
```

### Redis Restart Recovery

- **AOF persistence enabled**: `appendonly yes`, `appendfsync everysec`
- **RDB snapshots**: `save 60 1000` (snapshot every 60s if 1000+ writes)
- On restart: Redis replays AOF log, queues restored
- Worst case: lose last 1 second of data; workers re-process those URLs (idempotent)

### API Restart Recovery

- Stateless API: no in-memory state to lose
- Active crawlings tracked in Redis `active:crawlings` SET
- On restart: API reads from Redis/MongoDB, full state recovered
- In-flight URL ingestion (async goroutine) may need manual restart

### Network Partition

- Workers: detect Redis/MongoDB errors, back off and retry
- No URLs lost: tasks remain in Redis queues until processed
- Rate limiter: temporarily over-allows during partition (safe)

### MongoDB Failure

- Workers: crawl results buffered in goroutine, retried on reconnect
- URL ingestion: fails with error, job status set to "failed"
- Recovery: restart job with `reload_source: false` to resume from queue

---

## 12. Observability

### Prometheus Metrics

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `crawler_urls_crawled_total` | Counter | crawling_id, status | Total crawled URLs |
| `crawler_url_fetch_duration_seconds` | Histogram | crawling_id | Fetch latency distribution |
| `crawler_errors_total` | Counter | crawling_id, error_type | Error breakdown |
| `crawler_queue_pending` | Gauge | crawling_id | Queue backlog |
| `crawler_queue_processing` | Gauge | crawling_id | In-flight tasks |
| `crawler_queue_retry` | Gauge | crawling_id | Retry queue size |
| `crawler_queue_dead` | Gauge | crawling_id | Dead letter count |
| `crawler_workers_active` | Gauge | — | Active goroutines |
| `crawler_workers_idle` | Gauge | — | Idle goroutines |
| `crawler_rate_limit_tokens_acquired_total` | Counter | crawling_id | Token consumption |
| `crawler_rate_limit_waits_total` | Counter | crawling_id | Rate limit stalls |
| `crawler_http_status_codes_total` | Counter | crawling_id, code | Response codes |
| `crawler_active_crawlings` | Gauge | — | Active job count |
| `crawler_progress_percent` | Gauge | crawling_id | Job progress |

### Structured Logging

All logs are JSON-structured via zerolog:

```json
{
  "level": "info",
  "service": "crawler-worker",
  "worker_id": 42,
  "crawling_id": "65f1a2b3...",
  "url": "https://example.com/page",
  "status_code": 200,
  "response_time_ms": 234,
  "time": "2024-03-09T10:06:00.123Z"
}
```

### Grafana Dashboard

Pre-configured dashboard at `deployments/grafana/provisioning/dashboards/crawler-overview.json`:
- URLs Crawled Rate (by status)
- Active Workers gauge
- Active Crawlings gauge
- Fetch Latency (p50, p99)
- Queue Backlog (per crawling)
- Error Rate (by type)
- HTTP Status Codes

---

## 13. Project Structure

```
crawler-backend/
├── cmd/
│   ├── api/
│   │   └── main.go              # API service entrypoint
│   └── worker/
│       └── main.go              # Worker service entrypoint
├── internal/
│   ├── api/
│   │   └── handler.go           # HTTP handlers (sites, crawlings)
│   ├── config/
│   │   └── config.go            # Environment-based configuration
│   ├── crawler/
│   │   └── fetcher.go           # HTTP fetcher with connection pooling + domain limiter
│   ├── dedup/
│   │   └── dedup.go             # URL deduplication via Redis SET
│   ├── metrics/
│   │   └── metrics.go           # Prometheus metric definitions
│   ├── middleware/
│   │   └── middleware.go        # Gin middleware (logging, recovery, CORS)
│   ├── models/
│   │   └── models.go            # Domain models, DTOs, enums
│   ├── queue/
│   │   ├── queue.go             # Distributed queue (pending/processing/retry/dead)
│   │   └── jobstate.go          # Job state management + active crawlings registry
│   ├── ratelimiter/
│   │   └── ratelimiter.go       # Distributed token bucket rate limiter
│   ├── repository/
│   │   └── mongodb.go           # MongoDB operations with indexes
│   ├── source/
│   │   └── parser.go            # CSV/XML URL source parser
│   ├── webhook/
│   │   └── webhook.go           # Webhook event dispatcher with HMAC signing
│   └── worker/
│       └── pool.go              # Worker pool with recovery loop
├── deployments/
│   ├── prometheus/
│   │   └── prometheus.yml       # Prometheus scrape configuration
│   └── grafana/
│       └── provisioning/
│           ├── datasources/
│           │   └── datasource.yml
│           └── dashboards/
│               ├── dashboard.yml
│               └── crawler-overview.json
├── docker-compose.yml           # Full infrastructure stack
├── Dockerfile                   # Multi-stage build (API + Worker targets)
├── Makefile                     # Build, run, scale commands
├── go.mod
├── go.sum
├── .env.example
├── .gitignore
└── .dockerignore
```

### Module Responsibilities

| Module | Purpose |
|--------|---------|
| `cmd/api` | API server bootstrap, route registration, graceful shutdown |
| `cmd/worker` | Worker process bootstrap, pool management, graceful shutdown |
| `internal/api` | HTTP request handlers for all endpoints |
| `internal/config` | Environment variable parsing with defaults |
| `internal/crawler` | HTTP client with connection pooling, per-domain concurrency, redirects |
| `internal/dedup` | SHA-256 URL hashing + Redis SET deduplication |
| `internal/metrics` | Prometheus counter/gauge/histogram definitions |
| `internal/middleware` | Request logging, panic recovery, CORS |
| `internal/models` | All data structures (MongoDB docs, queue tasks, API DTOs) |
| `internal/queue` | Redis-based distributed queue with Lua scripts for atomicity |
| `internal/ratelimiter` | Distributed token bucket with Lua script |
| `internal/repository` | MongoDB CRUD with bulk operations and indexes |
| `internal/source` | CSV/XML file fetcher and URL extractor |
| `internal/webhook` | HTTP webhook delivery with HMAC-SHA256 signing |
| `internal/worker` | Goroutine pool, task processing, recovery loop |

---

## 14. Docker Compose Infrastructure

### Services

| Service | Image | Ports | Scaling |
|---------|-------|-------|---------|
| `api` | Custom (Go) | 8080, 9090 | Single instance |
| `worker` | Custom (Go) | 9090 (metrics) | `--scale worker=N` |
| `redis` | redis:7-alpine | 6379 | Single (or Redis Cluster for production) |
| `mongodb` | mongo:7 | 27017 | Single (or replica set for production) |
| `prometheus` | prom/prometheus | 9091 | Single |
| `grafana` | grafana/grafana | 3000 | Single |

### Commands

```bash
# Start everything with 2 workers (default)
docker compose up --build -d

# Scale to 10 workers
docker compose up -d --scale worker=10

# Scale to 20 workers
make scale N=20

# View logs
docker compose logs -f worker

# Stop everything
docker compose down

# Full cleanup (including volumes)
make clean
```

---

## 15. Performance Targets

### Target Metrics

| Metric | Target | How Achieved |
|--------|--------|-------------|
| Throughput | 10K+ URLs/sec | 10+ workers × 200 goroutines, ~50ms avg per URL |
| URLs per job | Millions | Streaming URL parser, batched enqueue, efficient Redis structures |
| Memory per worker | <512MB | Connection pooling, no full-file loading, goroutine-based (not thread) |
| Queue latency | <1ms | Redis in-memory, Lua scripts for atomic operations |
| Dequeue throughput | 100K+ ops/sec | Redis single-threaded but O(1) operations, pipelining |
| Job state propagation | <100ms | Workers poll Redis state every cycle |

### Bottleneck Analysis

| Component | Bottleneck | Mitigation |
|-----------|-----------|------------|
| Redis | Single-threaded | Lua scripts minimize round-trips; batch operations; Redis Cluster for >100K ops/sec |
| MongoDB | Write throughput | Bulk inserts, unordered writes, connection pooling, sharding |
| Network | Bandwidth | Workers are I/O-bound; add more containers on different hosts |
| Target sites | Response time | Per-domain concurrency limits, rate limiting prevents overwhelming targets |
| Rate limiter | Token generation | Distributed across Redis; sub-ms per acquire call |

---

## API Reference

### Sites

| Method | Endpoint | Description |
|--------|---------|-------------|
| POST | `/sites` | Create a new site |
| GET | `/sites` | List all sites (paginated) |
| GET | `/sites/:id` | Get site details |
| DELETE | `/sites/:id` | Delete a site |

### Crawlings

| Method | Endpoint | Description |
|--------|---------|-------------|
| POST | `/crawlings/start` | Start a new crawl job |
| GET | `/crawlings` | List crawl jobs (filterable) |
| GET | `/crawlings/:id` | Get crawl job details |
| POST | `/crawlings/:id/pause` | Pause a running crawl |
| POST | `/crawlings/:id/resume` | Resume a paused crawl |
| POST | `/crawlings/:id/stop` | Stop a crawl (cleanup queues) |
| GET | `/crawlings/:id/progress` | Get real-time progress + queue stats |
| GET | `/crawlings/:id/failures` | List failed URLs |

### Health

| Method | Endpoint | Description |
|--------|---------|-------------|
| GET | `/health` | Service health check |

---

## Data Flow Diagram

```
  Client                  API                    Redis                Workers              MongoDB
    │                      │                       │                     │                    │
    │  POST /sites         │                       │                     │                    │
    │─────────────────────▶│                       │                     │                    │
    │                      │───────── InsertOne ──────────────────────────────────────────────▶│
    │  201 Created         │                       │                     │                    │
    │◀─────────────────────│                       │                     │                    │
    │                      │                       │                     │                    │
    │  POST /crawlings/start                       │                     │                    │
    │─────────────────────▶│                       │                     │                    │
    │                      │───────── InsertOne(crawling) ───────────────────────────────────▶│
    │  202 Accepted        │                       │                     │                    │
    │◀─────────────────────│                       │                     │                    │
    │                      │                       │                     │                    │
    │                      │  [async goroutine]    │                     │                    │
    │                      │  1. Fetch source CSV/XML                    │                    │
    │                      │  2. Parse URLs         │                     │                    │
    │                      │  3. Dedup via SADD ───▶│                     │                    │
    │                      │  4. LPUSH to pending ─▶│                     │                    │
    │                      │  5. Init rate limiter ▶│                     │                    │
    │                      │  6. Set state=running ▶│                     │                    │
    │                      │                       │                     │                    │
    │                      │                       │  poll active jobs   │                    │
    │                      │                       │◀────────────────────│                    │
    │                      │                       │  acquire token      │                    │
    │                      │                       │◀────────────────────│                    │
    │                      │                       │  RPOP+ZADD (Lua)   │                    │
    │                      │                       │◀────────────────────│                    │
    │                      │                       │  return task        │                    │
    │                      │                       │───────────────────▶ │                    │
    │                      │                       │                     │  HTTP GET url      │
    │                      │                       │                     │──────────▶ target   │
    │                      │                       │                     │◀────────── response │
    │                      │                       │                     │                    │
    │                      │                       │  ZREM (ack)        │                    │
    │                      │                       │◀────────────────────│                    │
    │                      │                       │                     │──── InsertOne ────▶│
    │                      │                       │                     │   (result)         │
    │                      │                       │                     │──── UpdateOne ────▶│
    │                      │                       │                     │   (progress)       │
    │                      │                       │                     │                    │
    │  GET /progress       │                       │                     │                    │
    │─────────────────────▶│                       │                     │                    │
    │                      │◀──────── FindOne(crawling) ────────────────────────────────────── │
    │                      │◀──── queue stats ─────│                     │                    │
    │  200 progress JSON   │                       │                     │                    │
    │◀─────────────────────│                       │                     │                    │
```
