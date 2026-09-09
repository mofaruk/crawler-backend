package api

import (
	"sync"
	"time"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

// issueCacheTTL is how long a site's classified issues are reused.
//
// The underlying answer only changes when a crawl finishes, so this could be
// far longer; a minute is short enough that a finished crawl shows up while
// someone is still looking at the page, and long enough that drawing a row of
// badges costs one computation rather than one per site per render.
const issueCacheTTL = time.Minute

// issueCache memoises the site-issues computation.
//
// Two things matter here, and only the first is a cache. Classifying a site's
// issues groups every result in the window down to one row per URL and runs
// each through the classifier — seconds on a site with a long history. The
// dashboard asks for every site on the page at once, so without collapsing
// concurrent callers the same expensive query ran N times in parallel and the
// page waited for the slowest of them.
type issueCache struct {
	mu      sync.Mutex
	entries map[string]*issueCacheEntry
}

type issueCacheEntry struct {
	// ready is closed when the computation finishes; concurrent callers wait
	// on it rather than starting their own.
	ready chan struct{}

	issues   []models.SiteIssue
	total    int
	err      error
	computed time.Time
}

func newIssueCache() *issueCache {
	return &issueCache{entries: map[string]*issueCacheEntry{}}
}

// get returns the cached value for key, computing it if it is missing or
// stale. Callers arriving while a computation is in flight wait for it instead
// of duplicating the work.
func (c *issueCache) get(key string, compute func() ([]models.SiteIssue, int, error)) ([]models.SiteIssue, int, error) {
	c.mu.Lock()

	if entry, ok := c.entries[key]; ok {
		select {
		case <-entry.ready:
			// Finished. Reuse it while it is fresh; a failed computation is
			// not cached, so an error is always retried.
			if entry.err == nil && time.Since(entry.computed) < issueCacheTTL {
				c.mu.Unlock()

				return entry.issues, entry.total, nil
			}
		default:
			// Still running — wait for whoever started it.
			c.mu.Unlock()
			<-entry.ready

			return entry.issues, entry.total, entry.err
		}
	}

	entry := &issueCacheEntry{ready: make(chan struct{})}
	c.entries[key] = entry

	// Bound the map. These entries are small and the key space is one per
	// site and window, but a long-lived process should not accumulate them
	// without limit; dropping finished entries is safe because a dropped one
	// is simply recomputed.
	if len(c.entries) > issueCacheMaxEntries {
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

	entry.issues, entry.total, entry.err = compute()
	entry.computed = time.Now()
	close(entry.ready)

	return entry.issues, entry.total, entry.err
}

// issueCacheMaxEntries caps the cache. One entry per site per distinct window,
// and the dashboard uses a handful of windows, so this is far above normal use.
const issueCacheMaxEntries = 500
