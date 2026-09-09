package api

import (
	"sync"
	"time"
)

// resultCacheTTL is how long a computed answer is reused.
//
// The underlying answer only changes when a crawl finishes, so this could be
// far longer; a minute is short enough that a finished crawl shows up while
// someone is still looking at the page, and long enough that drawing a row of
// badges costs one computation rather than one per site per render.
const resultCacheTTL = time.Minute

// resultCache memoises an expensive per-site computation.
//
// Two things matter here, and only the first is a cache. Classifying issues or
// aggregating a window of results takes seconds on a site with a long history,
// and the dashboard asks for every site on a page at once — so without
// collapsing concurrent callers the same expensive query ran N times in
// parallel and the page waited for the slowest of them.
type resultCache struct {
	mu      sync.Mutex
	entries map[string]*resultCacheEntry
}

type resultCacheEntry struct {
	// ready is closed when the computation finishes; concurrent callers wait
	// on it rather than starting their own.
	ready chan struct{}

	value    any
	err      error
	computed time.Time
}

func newResultCache() *resultCache {
	return &resultCache{entries: map[string]*resultCacheEntry{}}
}

// get returns the cached value for key, computing it if it is missing or
// stale. Callers arriving while a computation is in flight wait for it instead
// of duplicating the work.
func (c *resultCache) get(key string, compute func() (any, error)) (any, error) {
	c.mu.Lock()

	if entry, ok := c.entries[key]; ok {
		select {
		case <-entry.ready:
			// Finished. Reuse it while it is fresh; a failed computation is
			// not cached, so an error is always retried.
			if entry.err == nil && time.Since(entry.computed) < resultCacheTTL {
				c.mu.Unlock()

				return entry.value, nil
			}
		default:
			// Still running — wait for whoever started it.
			c.mu.Unlock()
			<-entry.ready

			return entry.value, entry.err
		}
	}

	entry := &resultCacheEntry{ready: make(chan struct{})}
	c.entries[key] = entry

	// Bound the map. These entries are small and the key space is one per
	// site and window, but a long-lived process should not accumulate them
	// without limit; dropping finished entries is safe because a dropped one
	// is simply recomputed.
	if len(c.entries) > resultCacheMaxEntries {
		for k, e := range c.entries {
			if k == key {
				continue
			}
			select {
			case <-e.ready:
				delete(c.entries, k)
			default:
			}
		}
	}

	c.mu.Unlock()

	entry.value, entry.err = compute()
	entry.computed = time.Now()
	close(entry.ready)

	return entry.value, entry.err
}

// resultCacheMaxEntries caps the cache. One entry per site per distinct window,
// and the dashboard uses a handful of windows, so this is far above normal use.
const resultCacheMaxEntries = 500
