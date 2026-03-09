# How to Use - Crawler Backend

## Table of Contents

- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [Docker Compose Commands](#docker-compose-commands)
- [Scaling Workers](#scaling-workers)
- [Environment Configuration](#environment-configuration)
- [API Usage Guide](#api-usage-guide)
  - [Health Check](#health-check)
  - [Sites](#sites)
  - [Crawlings](#crawlings)
- [Complete Crawling Workflow](#complete-crawling-workflow)
- [Monitoring](#monitoring)
- [Troubleshooting](#troubleshooting)

---

## Prerequisites

- Docker Engine 24+
- Docker Compose v2+
- curl (for testing) or Postman (collection provided)

---

## Quick Start

```bash
# 1. Clone the repository
git clone <repo-url>
cd crawler-backend

# 2. Start the full stack (API + 2 workers + Redis + MongoDB + Prometheus + Grafana)
docker compose up --build -d

# 3. Verify everything is running
docker compose ps

# 4. Check API health
curl http://localhost:8088/health
```

Expected response:
```json
{"service": "api", "status": "ok"}
```

---

## Docker Compose Commands

### Start the system

```bash
# Start with default 2 workers
docker compose up --build -d

# Start with custom number of workers
docker compose up --build -d --scale crawler_worker=10
```

### Stop the system

```bash
# Stop all containers (data preserved in volumes)
docker compose down

# Stop and DELETE all data (volumes removed)
docker compose down -v
```

### View logs

```bash
# All services
docker compose logs -f

# API only
docker compose logs -f crawler_api

# Workers only
docker compose logs -f crawler_worker

# Follow last 100 lines
docker compose logs -f --tail=100 crawler_worker
```

### Restart a service

```bash
docker compose restart crawler_api
docker compose restart crawler_worker
```

---

## Scaling Workers

Workers are the crawling engines. Each worker container runs N goroutines (default: 200). Scale workers to increase throughput.

```bash
# Scale to 5 workers (5 x 200 = 1,000 goroutines)
docker compose up -d --scale crawler_worker=5

# Scale to 20 workers (20 x 200 = 4,000 goroutines)
docker compose up -d --scale crawler_worker=20

# Scale back down to 2
docker compose up -d --scale crawler_worker=2

# Using Makefile shorthand
make scale N=10
```

### Throughput estimates

| Workers | Goroutines | Estimated Throughput |
|---------|------------|---------------------|
| 1       | 200        | ~1,000 URLs/sec     |
| 5       | 1,000      | ~5,000 URLs/sec     |
| 10      | 2,000      | ~10,000 URLs/sec    |
| 20      | 4,000      | ~20,000 URLs/sec    |

> Note: Actual throughput depends on target site response times and your configured `speed` (rate limit).

### Change goroutines per worker

Edit `WORKER_CONCURRENCY` in `docker-compose.yml` or override at runtime:

```bash
WORKER_CONCURRENCY=500 docker compose up -d --scale crawler_worker=10
```

---

## Environment Configuration

All configuration is via environment variables. See `.env.example` for the full list.

### Key variables

| Variable | Default | Description |
|----------|---------|-------------|
| `API_PORT` | 8080 | HTTP API port |
| `METRICS_PORT` | 9090 | Prometheus metrics port |
| `WORKER_CONCURRENCY` | 200 | Goroutines per worker container |
| `WORKER_POLL_INTERVAL` | 100ms | How often workers check for tasks |
| `CRAWLER_TIMEOUT` | 30s | HTTP timeout per URL |
| `CRAWLER_MAX_RETRIES` | 3 | Retry attempts for failed URLs |
| `DEFAULT_USER_AGENT` | WK-Crawler/1.0 | Default crawler user agent |
| `LOG_LEVEL` | info | Logging level (debug/info/warn/error) |

---

## API Usage Guide

**Base URL:** `http://localhost:8088`

### Health Check

```bash
curl http://localhost:8088/health
```

---

### Sites

A site must be created before starting a crawl.

#### Create a site

```bash
curl -X POST http://localhost:8088/sites \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Example Blog",
    "base_url": "https://example.com",
    "url_limit": 5000,
    "url_source": "https://example.com/sitemap.xml",
    "url_source_type": "xml",
    "user_agent": "WK-Crawler/1.0",
    "extract_data": "Content-Type, Server, X-Powered-By"
  }'
```

**Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Friendly name for the site |
| `base_url` | string | Yes | Base URL of the site (must be a valid URL) |
| `url_limit` | integer | Yes | Maximum number of URLs to crawl (min: 1) |
| `url_source` | string | Yes | Public URL to a CSV or XML sitemap file |
| `url_source_type` | string | Yes | `csv` or `xml` |
| `user_agent` | string | No | Custom user agent (default: WK-Crawler/1.0) |
| `extract_data` | string | No | Comma-separated HTTP header names to extract |

**Response (201):**
```json
{
  "id": "65f1a2b0c1d2e3f4a5b6c7d8",
  "name": "Example Blog",
  "base_url": "https://example.com",
  "url_limit": 5000,
  "url_source": "https://example.com/sitemap.xml",
  "url_source_type": "xml",
  "user_agent": "WK-Crawler/1.0",
  "extract_data": ["Content-Type", "Server", "X-Powered-By"],
  "created_at": "2024-03-09T10:00:00Z",
  "updated_at": "2024-03-09T10:00:00Z"
}
```

> Save the `id` -- you need it to start a crawl.

#### CSV source format

The CSV file must have URLs in the **first column**. Header rows (`url`, `URL`) are skipped automatically.

```csv
url
https://example.com/page-1
https://example.com/page-2
https://example.com/page-3
```

#### XML source format

Standard sitemap XML format:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/page-1</loc></url>
  <url><loc>https://example.com/page-2</loc></url>
</urlset>
```

#### Update a site

Update one or more fields of an existing site. Only include the fields you want to change.

```bash
curl -X PUT http://localhost:8088/sites/65f1a2b0c1d2e3f4a5b6c7d8 \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Updated Blog Name",
    "url_limit": 10000,
    "extract_data": "Content-Type, Server, X-Cache"
  }'
```

**Parameters (all optional):**

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Friendly name for the site |
| `base_url` | string | Base URL of the site (must be a valid URL) |
| `url_limit` | integer | Maximum number of URLs to crawl (min: 1) |
| `url_source` | string | Public URL to a CSV or XML sitemap file |
| `url_source_type` | string | `csv` or `xml` |
| `user_agent` | string | Custom user agent |
| `extract_data` | string | Comma-separated HTTP header names to extract |

**Response (200):**
```json
{
  "id": "65f1a2b0c1d2e3f4a5b6c7d8",
  "name": "Updated Blog Name",
  "base_url": "https://example.com",
  "url_limit": 10000,
  "url_source": "https://example.com/sitemap.xml",
  "url_source_type": "xml",
  "user_agent": "WK-Crawler/1.0",
  "extract_data": ["Content-Type", "Server", "X-Cache"],
  "created_at": "2024-03-09T10:00:00Z",
  "updated_at": "2024-03-09T12:30:00Z"
}
```

#### List all sites

```bash
curl http://localhost:8088/sites

# With pagination
curl "http://localhost:8088/sites?skip=0&limit=10"
```

#### Get a single site

```bash
curl http://localhost:8088/sites/65f1a2b0c1d2e3f4a5b6c7d8
```

#### Delete a site

```bash
curl -X DELETE http://localhost:8088/sites/65f1a2b0c1d2e3f4a5b6c7d8
```

---

### Crawlings

#### Start a crawl

```bash
curl -X POST http://localhost:8088/crawlings/start \
  -H "Content-Type: application/json" \
  -d '{
    "site_id": "65f1a2b0c1d2e3f4a5b6c7d8",
    "speed": 36000,
    "reload_source": false
  }'
```

**Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `site_id` | string | Yes | The site ID from the create site response |
| `speed` | integer | No | URLs per hour (default: 3600, max: 72000) |
| `reload_source` | boolean | No | Re-fetch the URL source file (default: false) |

**Speed examples:**

| Speed | Rate |
|-------|------|
| 3600 | 1 URL/sec |
| 36000 | 10 URLs/sec |
| 72000 | 20 URLs/sec (max) |

**Response (202):**
```json
{
  "id": "65f1a2b3c1d2e3f4a5b6c7d9",
  "status": "pending",
  "message": "crawling job created, URL ingestion started"
}
```

The job starts in `pending` state. Once URLs are parsed and enqueued, it transitions to `running`.

> Save the crawling `id` -- you need it for all lifecycle operations.

#### Check crawl progress

```bash
curl http://localhost:8088/crawlings/65f1a2b3c1d2e3f4a5b6c7d9/progress
```

**Response:**
```json
{
  "id": "65f1a2b3c1d2e3f4a5b6c7d9",
  "site_id": "65f1a2b0c1d2e3f4a5b6c7d8",
  "status": "running",
  "total_urls": 4850,
  "crawled_urls": 1230,
  "failed_urls": 5,
  "progress": 25.46,
  "speed": 36000,
  "started_at": "2024-03-09T10:05:00Z",
  "created_at": "2024-03-09T10:04:55Z",
  "queue": {
    "pending": 3615,
    "processing": 12,
    "retry": 3,
    "dead": 0
  }
}
```

#### Pause a crawl

```bash
curl -X POST http://localhost:8088/crawlings/65f1a2b3c1d2e3f4a5b6c7d9/pause
```

Workers stop picking up new tasks within milliseconds. In-flight requests complete normally.

**Response:**
```json
{
  "id": "65f1a2b3c1d2e3f4a5b6c7d9",
  "status": "paused"
}
```

#### Resume a crawl

```bash
curl -X POST http://localhost:8088/crawlings/65f1a2b3c1d2e3f4a5b6c7d9/resume
```

**Response:**
```json
{
  "id": "65f1a2b3c1d2e3f4a5b6c7d9",
  "status": "running"
}
```

#### Stop a crawl

Stops the crawl and **cleans up all queues**. This is irreversible -- remaining URLs are discarded.

```bash
curl -X POST http://localhost:8088/crawlings/65f1a2b3c1d2e3f4a5b6c7d9/stop
```

**Response:**
```json
{
  "id": "65f1a2b3c1d2e3f4a5b6c7d9",
  "status": "stopped"
}
```

#### List all crawlings

```bash
# All crawlings
curl http://localhost:8088/crawlings

# Filter by site
curl "http://localhost:8088/crawlings?site_id=65f1a2b0c1d2e3f4a5b6c7d8"

# Filter by status
curl "http://localhost:8088/crawlings?status=running"

# With pagination
curl "http://localhost:8088/crawlings?skip=0&limit=50"
```

#### Get crawling details

```bash
curl http://localhost:8088/crawlings/65f1a2b3c1d2e3f4a5b6c7d9
```

#### Get crawl failures

```bash
curl "http://localhost:8088/crawlings/65f1a2b3c1d2e3f4a5b6c7d9/failures?skip=0&limit=20"
```

**Response:**
```json
{
  "data": [
    {
      "id": "65f1a500...",
      "crawling_id": "65f1a2b3...",
      "url": "https://example.com/broken",
      "error": "connection timeout after 30s",
      "status_code": 0,
      "retries": 3,
      "failed_at": "2024-03-09T10:10:00Z"
    }
  ]
}
```

---

## Complete Crawling Workflow

Here is a full end-to-end example:

```bash
# 1. Start the system
docker compose up --build -d --scale crawler_worker=5

# 2. Create a site
SITE_ID=$(curl -s -X POST http://localhost:8088/sites \
  -H "Content-Type: application/json" \
  -d '{
    "name": "My Website",
    "base_url": "https://mysite.com",
    "url_limit": 10000,
    "url_source": "https://mysite.com/sitemap.xml",
    "url_source_type": "xml",
    "extract_data": "Content-Type, Server"
  }' | jq -r '.id')

echo "Site ID: $SITE_ID"

# 3. Start a crawl at 10 URLs/sec
CRAWL_ID=$(curl -s -X POST http://localhost:8088/crawlings/start \
  -H "Content-Type: application/json" \
  -d "{
    \"site_id\": \"$SITE_ID\",
    \"speed\": 36000
  }" | jq -r '.id')

echo "Crawling ID: $CRAWL_ID"

# 4. Monitor progress (repeat this)
curl -s http://localhost:8088/crawlings/$CRAWL_ID/progress | jq .

# 5. Pause if needed
curl -s -X POST http://localhost:8088/crawlings/$CRAWL_ID/pause | jq .

# 6. Resume
curl -s -X POST http://localhost:8088/crawlings/$CRAWL_ID/resume | jq .

# 7. Or stop entirely
curl -s -X POST http://localhost:8088/crawlings/$CRAWL_ID/stop | jq .
```

### Poll progress in a loop

```bash
while true; do
  curl -s http://localhost:8088/crawlings/$CRAWL_ID/progress | jq '{status, progress, crawled_urls, total_urls, queue}'
  sleep 5
done
```

---

## Monitoring

### Grafana Dashboard

Open **http://localhost:3030** in your browser.

- Username: `admin`
- Password: `admin`

A pre-configured "Crawler Overview" dashboard shows:
- URLs crawled per second (by success/failure)
- Active workers
- Active crawling jobs
- Fetch latency (p50, p99)
- Queue backlog per crawling job
- Error rate by type
- HTTP response status codes

### Prometheus

Open **http://localhost:9095** to query metrics directly.

Example queries:

```promql
# Crawl throughput (URLs/sec)
sum(rate(crawler_urls_crawled_total[1m]))

# Error rate
sum(rate(crawler_errors_total[5m]))

# Queue backlog
sum(crawler_queue_pending)

# Active workers across all containers
sum(crawler_workers_active)

# p99 fetch latency
histogram_quantile(0.99, sum(rate(crawler_url_fetch_duration_seconds_bucket[5m])) by (le))
```

### Raw metrics endpoint

```bash
# API metrics
curl http://localhost:9098/metrics

# Worker metrics (only accessible from within Docker network)
```

---

## Troubleshooting

### System won't start

```bash
# Check container statuses
docker compose ps

# Check if Redis and MongoDB are healthy
docker compose logs crawler_redis
docker compose logs crawler_mongodb
```

### Crawl stuck in "pending"

The job is still ingesting URLs from the source file. Check API logs:

```bash
docker compose logs -f crawler_api | grep "URL ingestion"
```

If the source URL is unreachable, the job will move to `failed` status.

### Crawl not progressing

1. Check workers are running: `docker compose ps crawler_worker`
2. Check worker logs: `docker compose logs -f crawler_worker`
3. Check queue stats via progress endpoint
4. Verify rate limit isn't too low (speed = 3600 means only 1 URL/sec)

### High failure rate

```bash
# Check failures
curl http://localhost:8088/crawlings/$CRAWL_ID/failures?limit=50 | jq .

# Common causes:
# - Target site blocking the crawler (change user_agent)
# - Target site rate limiting (reduce speed)
# - Network issues (check docker network)
```

### Redis out of memory

```bash
# Check Redis memory
docker compose exec crawler_redis redis-cli INFO memory

# If needed, increase maxmemory in docker-compose.yml
# Default is 1GB
```

### Reset everything

```bash
# Stop all containers and delete all data
docker compose down -v

# Start fresh
docker compose up --build -d
```

---

## Ports Reference

| Service | Port | URL |
|---------|------|-----|
| API | 8088 | http://localhost:8088 |
| API Metrics | 9098 | http://localhost:9098/metrics |
| Prometheus | 9095 | http://localhost:9095 |
| Grafana | 3030 | http://localhost:3030 |
| Redis | 6382 | redis://localhost:6382 |
| MongoDB | 27020 | mongodb://localhost:27020 |

---

## Postman Collection

Import the file `postman/crawler-backend.postman_collection.json` into Postman for a ready-to-use collection with all endpoints, example bodies, and variables.
