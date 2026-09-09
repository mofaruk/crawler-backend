package api

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

// orphanSweepInterval is how often the recovery sweep runs.
//
// Orphans are made by a deploy or a Redis restart, so this is about how long a
// customer's crawl sits dead before it is noticed rather than about load: the
// sweep is one indexed query plus a Redis key check per unfinished crawl.
const orphanSweepInterval = 5 * time.Minute

// orphanMinAge is how long a crawl must have been untouched before the sweep
// will touch it.
//
// A crawl that has just been created has a Mongo record before its Redis state
// exists, and one mid-ingestion has an empty queue for entirely normal
// reasons. Without this the sweep would restart healthy crawls seconds after
// they began, which is worse than the problem it fixes.
const orphanMinAge = 15 * time.Minute

// StartOrphanRecovery runs the recovery sweep until ctx is cancelled.
//
// A crawl's live state — its queue, its dedup set, its membership of the
// dispatcher's active set — lives in Redis, while the record of it lives in
// MongoDB. A deploy or a Redis restart drops the first and keeps the second,
// and the result is a crawl Mongo reports as running that no worker will ever
// look at again: the dispatcher only visits crawls in the active set, so the
// completion check that would finish it is never reached either.
//
// Two such crawls sat running for a month before anyone noticed, one of them
// with 14,598 URLs it never fetched. This finds them and starts them again.
//
// It runs once at startup — the common case is a deploy that just happened —
// and then on a timer, because Redis can also be lost while the service keeps
// running, and then no restart ever occurs to trigger a startup-only check.
func (h *Handler) StartOrphanRecovery(ctx context.Context) {
	go func() {
		h.recoverOrphanedCrawlings(ctx)

		ticker := time.NewTicker(orphanSweepInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.recoverOrphanedCrawlings(ctx)
			}
		}
	}()
}

// recoverOrphanedCrawlings restarts crawls Mongo believes are active but Redis
// has forgotten.
func (h *Handler) recoverOrphanedCrawlings(ctx context.Context) {
	// Only rounds Mongo still considers unfinished. A completed or stopped one
	// is not an orphan however long ago it ran.
	filter := bson.M{
		"status": bson.M{"$in": bson.A{
			models.CrawlStatusPending,
			models.CrawlStatusDiscovering,
			models.CrawlStatusRunning,
		}},
		"updated_at": bson.M{"$lt": time.Now().Add(-orphanMinAge)},
	}

	crawlings, _, err := h.repo.ListCrawlings(ctx, filter, 0, 100)
	if err != nil {
		log.Error().Err(err).Msg("orphan sweep: failed to list unfinished crawlings")

		return
	}

	for _, crawling := range crawlings {
		crawlingID := crawling.ID.Hex()

		// Redis is the authority on whether anything is still working on this.
		// A crawl with live state is either progressing or paused, and neither
		// is ours to interfere with.
		if _, err := h.stateManager.GetState(ctx, crawlingID); err == nil {
			continue
		}

		site, err := h.repo.GetSite(ctx, crawling.SiteID)
		if err != nil || site == nil {
			// The site is gone, so the round can never be finished. Marking it
			// failed stops it being swept forever.
			log.Warn().Str("crawling_id", crawlingID).Msg("orphan sweep: site no longer exists, failing the round")
			_ = h.repo.SetCrawlingStoppedReason(ctx, crawling.ID, "The site this round belonged to no longer exists.")
			_ = h.repo.UpdateCrawlingStatus(ctx, crawling.ID, models.CrawlStatusFailed)

			continue
		}

		log.Warn().
			Str("crawling_id", crawlingID).
			Str("site_id", crawling.SiteID.Hex()).
			Int("crawled", crawling.CrawledURLs).
			Int("total", crawling.TotalURLs).
			Time("started_at", crawling.CreatedAt).
			Msg("orphan sweep: crawl has no live state, restarting it")

		h.restartOrphan(ctx, crawling, site)
	}
}

// restartOrphan closes out a lost round and begins a fresh one for its site.
//
// Restarted rather than resumed: the pending queue is exactly what was lost, so
// resuming would mean rebuilding it from the source anyway — and smart recrawl
// already skips URLs whose last result is still cached, so a fresh round costs
// little more than a resumed one would while going through the ordinary start
// path rather than a second, half-tested one.
func (h *Handler) restartOrphan(ctx context.Context, orphan models.Crawling, site *models.Site) {
	_ = h.repo.SetCrawlingStoppedReason(ctx, orphan.ID,
		"This round lost its queue — usually a deploy or a restart of the crawler's Redis — and was restarted automatically.")

	if err := h.repo.UpdateCrawlingStatus(ctx, orphan.ID, models.CrawlStatusFailed); err != nil {
		log.Error().Err(err).Str("crawling_id", orphan.ID.Hex()).Msg("orphan sweep: failed to close out the lost round")

		return
	}

	// Any residue the lost round left behind. Best-effort: a leaked dedup set
	// is what once grew Redis to a gigabyte, and the replacement round needs a
	// clean one regardless.
	crawlingID := orphan.ID.Hex()
	_ = h.queue.DeleteQueue(ctx, crawlingID)
	_ = h.dedup.Cleanup(ctx, crawlingID)
	_ = h.rateLimiter.Cleanup(ctx, crawlingID)
	_ = h.stateManager.DeleteState(ctx, crawlingID)
	_ = h.stateManager.RemoveActiveCrawling(ctx, crawlingID)

	// Carry the lost round's settings, so a restart is the same crawl again
	// rather than one at default speed.
	replacement := &models.Crawling{
		SiteID:       orphan.SiteID,
		Status:       models.CrawlStatusPending,
		Speed:        orphan.Speed,
		AssetSpeed:   orphan.AssetSpeed,
		ReloadSource: orphan.ReloadSource,
		URLType:      orphan.URLType,
	}

	if err := h.repo.CreateCrawling(ctx, replacement); err != nil {
		log.Error().Err(err).Str("site_id", orphan.SiteID.Hex()).Msg("orphan sweep: failed to create the replacement round")

		return
	}

	log.Info().
		Str("replaced", crawlingID).
		Str("crawling_id", replacement.ID.Hex()).
		Msg("orphan sweep: started a replacement round")

	// Same ingestion path a manual start uses, so there is only one way a
	// crawl ever begins.
	go h.ingestURLs(replacement.ID.Hex(), site, replacement)
}
