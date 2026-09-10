# 4. A round that fetches nothing records nothing

Date: 2026-09-11

## Status

Accepted

## Context

Smart recrawl skips URLs whose last result is still cached and younger than
the site's re-check age, and carries the previous result forward into the new
round so that the round still reads as a complete report of the site. That is
deliberate: a customer opening a round should see every URL, not only the
few that happened to be re-fetched.

Both ingestion paths carried results forward *before* checking whether the
round would fetch anything. A round on a fully cached site therefore did this:
copy every previous result into itself, mark itself completed, and finish in
under a second. The dashboard scheduler runs every minute and, in continuous
mode, starts a new round as soon as the previous one has finished. The two
together produced one round per minute, all day: nlphuset.dk gained a hundred
rounds in under two hours, 92 of them never started, each writing 1,445 result
documents to record that nothing had changed. About two million rows a day,
containing no information, for one site.

## Decision

When ingestion finds nothing to fetch, the round is closed as `completed`
with `total_urls` and `crawled_urls` at zero, no results, and a
`stopped_reason` saying every URL was still cached. Results are carried
forward only into a round that fetches something.

The dashboard scheduler treats such a round as a reason to wait: continuous
mode has a fifteen-minute floor between rounds, and an hour after a round
that fetched nothing, since the answer cannot change until the cache ages
past the re-check limit.

## Alternatives considered

**Not creating the round at all.** The API creates the round before ingestion
runs, and the scheduler needs something to read back to know why nothing
happened. One small document is the right cost for that signal; 1,445 copied
results is not.

**A new `skipped` status.** Every status map in the dashboard, and every
query filtering on terminal statuses, would need updating. Zero totals on a
completed round carry the same meaning without a new vocabulary.

**Keeping the copy but throttling only in the scheduler.** Still writes a
full duplicate report on every manual "Crawl now" against a cached site, and
leaves the backend trusting every caller to be well behaved.

## Consequences

- A "completed" round can show 0/0 URLs. The dashboard shows the recorded
  reason beside it.
- Per-round analytics for such a round are empty; the previous round holds
  the data and is unchanged.
- The crawlings collection stops growing by a thousand rounds a day for a
  fully cached site; retention pruning has correspondingly less to do.
