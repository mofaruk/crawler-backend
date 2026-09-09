# Changelog

Notable changes to the crawl engine. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

Entries describe what changed for someone using or operating the product, not
how it was implemented — the commit says how, and the ADRs under `docs/adr/`
say why for decisions worth revisiting.

## [Unreleased]

### Security

- **The API now requires authentication.** `CRAWLER_API_KEY` was empty in
  production, which disables the check entirely, so every route except
  `/health` was readable and writable by anyone who knew the hostname —
  including `POST /crawlings/prune`, which deletes crawl data.

### Added

- Crawl results can be filtered by absence: `status_code_negate` and
  `value_negate` invert their matches, which is what "URLs not yet in the CDN
  cache that still return 200" requires. Negating a header value also matches
  URLs carrying no such header, since a page the CDN never saw is not in its
  cache either.
- `/sites` accepts `search` and `ids`, so a caller can page a filtered list
  instead of fetching a fixed window and narrowing it afterwards.
- Issues carry a `category` — `crawler` or `seo` — and the endpoint filters and
  counts by it. See [ADR 0002](docs/adr/0002-split-crawler-and-seo-issues.md).
- Crawls orphaned by a deploy are found and restarted automatically. See
  [ADR 0003](docs/adr/0003-restart-orphaned-crawls.md).

### Changed

- A cache bypass is reported only once it repeats. One bypass is natural — a
  cold URL, a purge, a request that happened to carry a cookie — and flagging
  every one buried the pages the CDN genuinely will never store.

### Fixed

- Two accounts holding their own site record for the same domain could crawl it
  at once, doubling the request rate against the customer's origin. The guard
  now matches on the normalised host rather than the site id.
- "No caching policy" was reported for sites that send one. A site only stores
  the headers its `extract_data` names, and a missing value was read as proof
  the origin sends none — dearbaby.dk was flagged on every page while serving
  `s-maxage=31536000, max-age=60`.
- A site's issue count reported the size of the page requested rather than the
  site's real total, so a dashboard badge asking for one issue always read 1.
- The site-issues query examined every result in the window to test `site_id`,
  because `crawling_results` indexed that field and `crawled_at` separately.
  2.2s to about 1ms.
- Rolling windows were anchored to the current instant, so two identical
  requests never resolved to the same bounds and nothing keyed on a window
  could be cached. See [ADR 0001](docs/adr/0001-cache-expensive-site-queries.md).

### Performance

- Site issues, timeline and analytics are computed once per window per minute
  rather than per request, and concurrent callers share one computation instead
  of each starting their own.
