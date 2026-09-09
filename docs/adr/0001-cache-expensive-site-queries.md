# 1. Cache expensive site queries in the API, keyed on the window

Date: 2026-09-09

## Status

Accepted.

## Context

Classifying a site's issues groups every crawl result in the window down to one
row per URL and runs each through the classifier. On dearbaby.dk — 18k results,
7.7k distinct URLs — that took seconds, and it scales with the window: 0.8s at
one day, 4s at seven, 7s at thirty. The timeline and analytics endpoints
aggregate the same collection and cost the same order.

The dashboard asks for every site on a page at once to draw a row of issue
badges, so the slowest site set the page's speed. The Sites page took 12s
against production; the site analytics page 5.6s.

An index on `(site_id, crawled_at)` removed the collection scan underneath, but
the aggregation itself is still proportional to the results in the window.

## Decision

Cache the computed answer in the API process, keyed on site and window, for one
minute. Callers arriving while a computation is in flight wait for it rather
than starting their own.

Rolling windows are truncated to the minute so that two identical requests
resolve to the same bounds. Anchored to the current instant they never did, and
the first version of this cache was consulted with a fresh key every time —
present, correct, and never once hit.

Failed computations are not cached, so an error is always retried.

## Consequences

A crawl that finishes is invisible for up to a minute. That is acceptable: the
numbers describe a window of days, and no decision a customer makes turns on
sixty seconds.

The cache is per process. Running several API replicas means each holds its
own, which is fine — it is a memoisation, not a source of truth — but it does
mean the hit rate falls as replicas are added.

Collapsing concurrent callers matters more than the cache itself. It is what
turns seven parallel badge requests into one computation, and it is why this
lives in the API rather than in front of it.
