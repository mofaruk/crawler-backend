# 3. Restart orphaned crawls rather than resume them

Date: 2026-09-09

## Status

Accepted.

## Context

A crawl's live state — its queue, its dedup set, its membership of the
dispatcher's active set — lives in Redis. The record of it lives in MongoDB. A
deploy or a Redis restart drops the first and keeps the second.

What is left is a crawl Mongo calls running that no worker will ever look at
again: the dispatcher only visits crawls in the active set, so even the
completion check that would end it is unreachable.

Two had been sitting that way for a month when this was found — one since 19
May, one since 9 August holding 14,598 URLs it never fetched. Quota a customer
paid for.

## Decision

Sweep for rounds Mongo still calls unfinished that Redis has no state for.
Close each with a reason the dashboard shows, and start a replacement carrying
the same speed and scope.

Restart rather than resume. The pending queue is exactly what was lost, so
resuming means rebuilding it from the source anyway — and smart recrawl already
skips URLs whose last result still holds, so a fresh round costs little more
while going through the ordinary start path rather than a second, half-tested
one.

The sweep runs in the API rather than the worker, because URL ingestion lives
there. It runs at startup, for the deploy that just happened, and every five
minutes, because Redis can also be lost while the service keeps running — and
then no restart occurs for a startup-only check to hook into.

## Consequences

A crawl untouched for less than fifteen minutes is left alone. A round
mid-ingestion has an empty queue for entirely ordinary reasons, and restarting
healthy crawls seconds after they begin would be worse than the problem.

A restarted round loses its partial progress in the record: the old one is
marked failed and a new one starts from zero. The URLs already warmed stay
warmed, and smart recrawl will skip them, but the crawled count restarts. That
is the price of not maintaining a second resume path.

Deploys no longer silently cost a customer a round. Nothing else changes about
how crawls are scheduled.
