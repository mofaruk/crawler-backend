package api

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/webkonsulenterne/crawler-backend/internal/linkcheck"
)

// linkSweepInterval is how often the scheduler looks for links to verify.
//
// Short relative to linkRecheckAge because each pass does a bounded amount of
// work: it is the batch size, not the interval, that decides how fast a
// backlog clears. A site with thousands of unchecked links is worked through
// over hours rather than hammering every destination at once.
const linkSweepInterval = 10 * time.Minute

// linkRecheckAge is how stale a link's last check must be before it is
// verified again.
//
// These are other people's servers. A destination that answered yesterday will
// almost certainly answer today, and re-asking more often than this is rude
// without being more useful.
const linkRecheckAge = 7 * 24 * time.Hour

// linkSweepBatch bounds how many links one pass checks per site.
//
// Deliberately small. The sweep runs unattended, so a pass that took minutes
// would overlap the next one and multiply the load on destinations that have
// done nothing wrong.
const linkSweepBatch = 50

// linkSweepSites bounds how many sites one pass touches.
const linkSweepSites = 20

// StartLinkCheckScheduler verifies outbound links on a timer until ctx is
// cancelled.
//
// Nothing ran the link checker before this. It fired only when someone opened
// a site's Links tab and pressed a button, so a report was accurate only for
// whoever had most recently thought to refresh it — dearbaby.dk sat with a
// thousand links that had never been checked at all, and a customer looking at
// that page had no way to tell "no broken links" from "nothing was ever
// looked at".
//
// The sweep is deliberately unhurried: a bounded batch per site per pass,
// nothing re-checked inside a week. It is a background chore, not a service
// the customer waits on.
func (h *Handler) StartLinkCheckScheduler(ctx context.Context) {
	go func() {
		// Not run at startup: a deploy restarts the process, and checking
		// every site's links on every deploy would turn a routine release into
		// a burst of traffic aimed at third parties.
		ticker := time.NewTicker(linkSweepInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.sweepOutboundLinks(ctx)
			}
		}
	}()
}

// sweepOutboundLinks checks one batch of stale links for each site in turn.
func (h *Handler) sweepOutboundLinks(ctx context.Context) {
	sites, _, err := h.repo.ListSites(ctx, 0, linkSweepSites)
	if err != nil {
		log.Error().Err(err).Msg("link sweep: failed to list sites")

		return
	}

	cutoff := time.Now().Add(-linkRecheckAge)

	for _, site := range sites {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pending, err := h.repo.OutboundLinksToCheck(ctx, site.ID, cutoff, linkSweepBatch)
		if err != nil {
			log.Warn().Err(err).Str("site_id", site.ID.Hex()).Msg("link sweep: failed to list links to check")

			continue
		}

		if len(pending) == 0 {
			continue
		}

		urls := make([]string, len(pending))
		for i, l := range pending {
			urls[i] = l.URL
		}

		// Its own deadline: one slow destination must not hold up every other
		// site's turn, and the sweep has nobody waiting on it to be prompt.
		batchCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)

		checker := linkcheck.New(site.UserAgent, 15*time.Second)
		results := checker.CheckAll(batchCtx, urls, h.cfg.LinkCheckConcurrency)

		broken := 0
		for i, res := range results {
			if err := h.repo.SaveLinkCheck(batchCtx, pending[i].ID, res.StatusCode, res.Error, res.ResponseTime.Milliseconds()); err != nil {
				log.Warn().Err(err).Str("url", res.URL).Msg("link sweep: failed to save link check")
			}

			// Counted the way the report counts, so this log line and the
			// customer's page cannot disagree.
			if res.Error != "" || (res.StatusCode >= 400 && !linkcheck.BotBlocked(res.StatusCode)) {
				broken++
			}
		}

		cancel()

		log.Info().
			Str("site_id", site.ID.Hex()).
			Int("checked", len(results)).
			Int("broken", broken).
			Msg("link sweep: checked a batch of outbound links")
	}
}
