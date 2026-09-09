# 2. Separate crawler issues from SEO ones

Date: 2026-09-09

## Status

Accepted.

## Context

The classifier emits 27 kinds of issue, from 404s and cache bypasses to missing
alt text and short titles. They were listed together.

One site reports 19,675 issues. The handful that stop a page being warmed — the
reason someone buys a cache crawler — were somewhere in the middle of them.

Jens put it plainly: crawler issues are the important ones for the SaaS, SEO
issues are a bonus for a webshop owner and not related to cache crawling. Worth
showing, not worth mixing in.

## Decision

Every issue carries a category, `crawler` or `seo`, decided by its kind. The
API filters and counts by it, and the dashboard lists them under two menu
points, each showing how many the other holds.

Availability and caching are crawler issues: unreachable, broken, gone, server
errors, redirects, blank pages, slow responses, and every cache state. Content
quality is SEO: titles, meta descriptions, canonicals, headings, alt text, thin
content, noindex, mixed content.

Two judgement calls worth recording. A soft 404 is a crawler issue rather than
an SEO one — warming a page that answers 200 while being gone fills the cache
with an error page. And a kind absent from the map defaults to `crawler`:
over-reporting in the list people watch is a smaller mistake than hiding
something there.

## Consequences

A new check must be categorised deliberately. A test asserts every kind the
classifier emits is mapped, and it immediately caught three that were not.

The split is a product claim, not just a filter: it says these findings are
what you are paying for and those are extra. If that stops being true — if SEO
findings become a sold feature — this needs revisiting rather than extending.

Counts that used to mean "all issues" now mean one category. The Sites badge
was changed to count crawler issues only, since it links to that list and a
badge disagreeing with its destination is worse than either number alone.
