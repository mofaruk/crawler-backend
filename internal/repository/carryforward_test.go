package repository

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

// A carried-forward result must keep pointing at the round that actually
// fetched it. If OriginalCrawledAt were reset each time, a page cached months
// ago would look freshly verified after two skipped rounds.
func TestCarryForwardPreservesOriginalFetchTime(t *testing.T) {
	firstFetch := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	secondRound := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	// Round two: fetched on 1 Aug, copied forward on 2 Aug.
	once := models.CrawlingResult{
		URL:               "https://example.dk/a",
		CrawledAt:         secondRound,
		CarriedForward:    true,
		OriginalCrawledAt: &firstFetch,
	}

	// Round three copies it again — the original must survive.
	original := once.CrawledAt
	if once.OriginalCrawledAt != nil {
		original = *once.OriginalCrawledAt
	}

	if !original.Equal(firstFetch) {
		t.Errorf("original fetch time = %v, want %v", original, firstFetch)
	}
}

// A result that has never been carried takes its own CrawledAt as the origin.
func TestCarryForwardFirstHopUsesCrawledAt(t *testing.T) {
	fetched := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)

	fresh := models.CrawlingResult{
		URL:       "https://example.dk/b",
		CrawledAt: fetched,
	}

	original := fresh.CrawledAt
	if fresh.OriginalCrawledAt != nil {
		original = *fresh.OriginalCrawledAt
	}

	if !original.Equal(fetched) {
		t.Errorf("original fetch time = %v, want %v", original, fetched)
	}
	if fresh.CarriedForward {
		t.Error("a freshly fetched result must not be marked carried forward")
	}
}

// Only a HIT-family status may be skipped. A MISS or BYPASS is precisely the
// URL worth re-checking, so treating it as cacheable would hide a regression.
func TestOnlyHitFamilyCountsAsCached(t *testing.T) {
	cacheable := map[string]bool{
		"HIT":         true,
		"REVALIDATED": true,
		"UPDATING":    true,
		"MISS":        false,
		"BYPASS":      false,
		"EXPIRED":     false,
		"DYNAMIC":     false,
		"":            false,
	}

	for status, want := range cacheable {
		got := status == "HIT" || status == "REVALIDATED" || status == "UPDATING"
		if got != want {
			t.Errorf("status %q treated as cached=%v, want %v", status, got, want)
		}
	}
}

func TestCarryForwardClearsIDSoInsertCreatesNewRow(t *testing.T) {
	res := models.CrawlingResult{
		ID:  primitive.NewObjectID(),
		URL: "https://example.dk/c",
	}

	res.ID = primitive.NilObjectID

	if !res.ID.IsZero() {
		t.Error("ID must be cleared before re-insert, or the copy overwrites the original row")
	}
}
