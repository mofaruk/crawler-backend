package repository

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

// siteTimeline stores one datapoint per completed round.
//
// The timeline used to be aggregated from crawling_results on every request,
// which meant the graph could only reach as far back as the raw per-URL rows
// were kept. Retention then forced a choice between a growing database and a
// short history. Rolling each round up before its results are pruned removes
// that trade: a round costs six numbers here instead of a couple of thousand
// documents there.
func (r *MongoRepository) siteTimeline() *mongo.Collection {
	return r.db.Collection("site_timeline")
}

// SaveTimelinePoints records the rolled-up numbers for a set of rounds.
//
// Idempotent per crawling: re-running a roll-up for a round that already has
// one replaces it rather than adding a second, so a retried prune cannot
// double a site's history.
func (r *MongoRepository) SaveTimelinePoints(
	ctx context.Context,
	siteID primitive.ObjectID,
	points []models.TimelinePoint,
) error {
	if len(points) == 0 {
		return nil
	}

	writes := make([]mongo.WriteModel, 0, len(points))

	for _, point := range points {
		point.SiteID = siteID

		writes = append(writes, mongo.NewReplaceOneModel().
			SetFilter(bson.M{"crawling_id": point.CrawlingID}).
			SetReplacement(point).
			SetUpsert(true))
	}

	_, err := r.siteTimeline().BulkWrite(ctx, writes, options.BulkWrite().SetOrdered(false))

	return err
}

// RollUpCrawlings builds timeline points for a site's rounds that started
// before a cutoff and do not have one yet.
//
// Called immediately before pruning: the results still exist at that moment,
// and this is the last chance to reduce them to something worth keeping.
func (r *MongoRepository) RollUpCrawlings(
	ctx context.Context,
	siteID primitive.ObjectID,
	before time.Time,
) (int, error) {
	// Everything the timeline aggregation would compute, for rounds about to
	// lose their results. Reuses GetSiteTimeline's own query so a stored point
	// and a computed one can never disagree.
	points, err := r.GetSiteTimeline(ctx, siteID, time.Time{}, 10000)
	if err != nil {
		return 0, err
	}

	var expiring []models.TimelinePoint
	for _, point := range points {
		if point.CrawledAt.Before(before) {
			expiring = append(expiring, point)
		}
	}

	if len(expiring) == 0 {
		return 0, nil
	}

	if err := r.SaveTimelinePoints(ctx, siteID, expiring); err != nil {
		return 0, err
	}

	return len(expiring), nil
}

// StoredTimeline returns the rolled-up points for a site, newest last.
func (r *MongoRepository) StoredTimeline(
	ctx context.Context,
	siteID primitive.ObjectID,
	since time.Time,
	limit int64,
) ([]models.TimelinePoint, error) {
	cursor, err := r.siteTimeline().Find(ctx,
		bson.M{"site_id": siteID, "crawled_at": bson.M{"$gte": since}},
		options.Find().SetSort(bson.D{{Key: "crawled_at", Value: 1}}).SetLimit(limit),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	points := make([]models.TimelinePoint, 0)
	if err := cursor.All(ctx, &points); err != nil {
		return nil, err
	}

	return points, nil
}

// MergedTimeline is what the API serves: stored points for rounds whose
// results have been pruned, plus live ones for rounds that still have them.
//
// Live wins on conflict — a round still holding its results is the more
// current answer, and a stored point is only ever a copy of what that
// aggregation produced.
func (r *MongoRepository) MergedTimeline(
	ctx context.Context,
	siteID primitive.ObjectID,
	since time.Time,
	limit int64,
) ([]models.TimelinePoint, error) {
	stored, err := r.StoredTimeline(ctx, siteID, since, limit)
	if err != nil {
		return nil, err
	}

	live, err := r.GetSiteTimeline(ctx, siteID, since, limit)
	if err != nil {
		return nil, err
	}

	byCrawling := make(map[string]models.TimelinePoint, len(stored)+len(live))
	for _, point := range stored {
		byCrawling[point.CrawlingID] = point
	}
	for _, point := range live {
		byCrawling[point.CrawlingID] = point
	}

	merged := make([]models.TimelinePoint, 0, len(byCrawling))
	for _, point := range byCrawling {
		merged = append(merged, point)
	}

	// Oldest first, matching what GetSiteTimeline returns, because the chart
	// plots them in order.
	sortTimeline(merged)

	if int64(len(merged)) > limit {
		merged = merged[int64(len(merged))-limit:]
	}

	return merged, nil
}

func sortTimeline(points []models.TimelinePoint) {
	for i := 1; i < len(points); i++ {
		for j := i; j > 0 && points[j].CrawledAt.Before(points[j-1].CrawledAt); j-- {
			points[j], points[j-1] = points[j-1], points[j]
		}
	}
}
