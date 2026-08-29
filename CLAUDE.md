# crawler-backend

Go crawl engine for **CacheCrawler** (cachecrawler.net) — a commercial
multi-tenant SaaS that crawls customers' sites and reports the HTTP response
headers each URL returned, above all CDN cache headers (`cf-cache-status`,
`age`, `cache-control`). It answers: *is my CDN actually caching my site?*

The customer-facing UI is a **separate repo**, `../crawler-dashboard`
(Laravel 11 + Filament). See its own CLAUDE.md.

## Branches — read this first

**`develop` is canonical, not `main`.** `main` is a stale July snapshot with
only 8 of the 21 routes the dashboard calls; running the dashboard against it
silently breaks half the UI. Feature work is currently on **`links-finder`**
(branched from develop).

Always check `git branch --show-current` before analysing this repo.

## Architecture

- `cmd/api` — Gin HTTP API. `cmd/worker` — separate crawl worker binary.
- MongoDB stores sites, crawlings, results, failures. Redis holds the queue,
  rate-limit token buckets, dedup sets and job state.
- One **dispatcher** goroutine owns all Redis polling and fans tasks out to N
  consumers over a channel. Do not reintroduce per-worker polling — that was
  an O(workers x crawls) storm, fixed deliberately.
- The API has **no concept of tenants**. It is keyed by `site_id`; all
  ownership and quota logic lives in the dashboard.

## Security model

Every route except `/health` requires `X-API-Key` (or a Bearer token).
`ValidateTargetURL` rejects non-public crawl targets — without it the API is
an SSRF primitive, since `base_url` and `url_source` are user-supplied and
fetched server-side.

Two env vars disable protection and each logs a loud startup warning:
`API_KEY=""` (no auth) and `ALLOW_PRIVATE_TARGETS=true` (no SSRF guard).
**Both must be set correctly in production.** Local development needs
`ALLOW_PRIVATE_TARGETS=true` because sources live on `host.docker.internal`.

## Local development

```
docker compose up -d --build          # API on :8088, metrics :9098
curl -H "X-API-Key: $KEY" localhost:8088/sites
```

The key lives in `.env` (gitignored). Test fixtures in `sample-data/` (also
gitignored) are served to the crawler with
`cd sample-data && python3 -m http.server 9999`.

Go is not installed on the host — build and test in Docker:

```
docker run --rm -v "$PWD":/app -w /app golang:1.23-alpine go build ./...
docker run --rm --network host -v "$PWD":/app -w /app golang:1.23-alpine go test ./... -count=1
```

Queue tests need Redis on `localhost:6382` and skip cleanly without it.

## Conventions

- Comments explain **why**, not what. Several non-obvious decisions here look
  wrong without their rationale (LPUSH+RPOP is correct FIFO; blue rather than
  amber for MISS; the same colour ramp in both themes).
- Verify claims by running them. A prior review asserted the rate limiter
  stalls every crawl; testing the Lua directly showed fractional tokens
  accumulate correctly and it was fine.

## Known open issues

- Discovery (`auto` source) is untested: a channel-close race past 1024
  queued pages, and no early return once `url_limit` is reached.
- `webhook` package, `MarkSeenBatch`, `DequeueBatch`, `BulkInsertResults` and
  the `crawl_urls` collection are all dead code.
