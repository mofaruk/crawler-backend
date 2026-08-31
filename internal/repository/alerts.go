package repository

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

func (r *MongoRepository) alertEvents() *mongo.Collection {
	return r.db.Collection("alert_events")
}

// headerValue reads a header case-insensitively: servers vary in casing, and a
// map lookup is exact.
func headerValue(headers map[string]string, name string) string {
	if headers == nil {
		return ""
	}
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func lower(s string) string { return strings.ToLower(s) }

// SummariseRound reduces one crawl round to the numbers alerting compares.
//
// Everything comes from stored results, so detecting a regression costs one
// aggregation rather than another crawl — the same principle the issue
// classifier follows.
func (r *MongoRepository) SummariseRound(
	ctx context.Context,
	crawlingID primitive.ObjectID,
) (*models.RoundSummary, error) {
	cursor, err := r.crawlingResults().Find(ctx,
		bson.M{"crawling_id": crawlingID},
		options.Find().SetProjection(bson.M{
			"url": 1, "status_code": 1, "response_time_ms": 1, "headers": 1, "page": 1,
			"redirected_to": 1, "content_type": 1, "crawled_at": 1,
		}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	summary := &models.RoundSummary{
		CrawlingID: crawlingID.Hex(),
		BrokenURLs: map[string]int{},
		IssueKinds: map[string]int{},
	}

	var (
		responseTimes []int64
		cached        int
		cacheKnown    int
		titleCounts   = map[string]int{}
		states        []models.URLState
	)

	for cursor.Next(ctx) {
		var res models.CrawlingResult
		if err := cursor.Decode(&res); err != nil {
			return nil, err
		}

		summary.URLs++

		// A 4xx/5xx, or no response at all, is "broken" for alerting. Keeping
		// the status code lets the alert say what kind of broken.
		if res.StatusCode >= 400 || res.StatusCode == 0 {
			summary.BrokenURLs[res.URL] = res.StatusCode
		}

		if res.ResponseTime > 0 {
			responseTimes = append(responseTimes, res.ResponseTime)
		}

		if status := headerValue(res.Headers, "CF-Cache-Status"); status != "" {
			cacheKnown++
			switch status {
			case "HIT", "REVALIDATED", "UPDATING", "hit", "revalidated", "updating":
				cached++
			}
		}

		if res.Page != nil && res.Page.Title != "" {
			titleCounts[lower(res.Page.Title)]++
		}

		states = append(states, models.URLState{
			URL:          res.URL,
			StatusCode:   res.StatusCode,
			ContentType:  res.ContentType,
			ResponseTime: res.ResponseTime,
			RedirectedTo: res.RedirectedTo,
			Headers:      res.Headers,
			Page:         res.Page,
			FirstSeen:    res.CrawledAt,
			LastSeen:     res.CrawledAt,
			Occurrences:  1,
		})
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}

	if summary.URLs == 0 {
		return nil, nil
	}

	if cacheKnown > 0 {
		summary.CachePercent = float64(int(float64(cached)/float64(cacheKnown)*1000)) / 10
	}

	// Median, not mean: one pathological page should not move a whole site's
	// number, which is the same reason the timeline uses a median age.
	if len(responseTimes) > 0 {
		sort.Slice(responseTimes, func(i, j int) bool { return responseTimes[i] < responseTimes[j] })
		summary.MedianResponseMs = responseTimes[len(responseTimes)/2]
	}

	// Reuse the issue classifier rather than restating what counts as a
	// problem: alerting reports that a kind appeared, the classifier decides
	// what the kinds are.
	for _, state := range states {
		for _, issue := range models.ClassifyURL(state, titleCounts) {
			summary.IssueKinds[issue.Kind]++
		}
	}

	return summary, nil
}

// PreviousCompletedCrawling returns the most recent completed round for a site
// other than the one given, which is what a finished round is compared against.
//
// Returns nil when there is none: a site's first round has nothing to compare
// to, and the detector treats that as "no alerts" rather than as an error.
func (r *MongoRepository) PreviousCompletedCrawling(
	ctx context.Context,
	siteID primitive.ObjectID,
	excludeCrawlingID primitive.ObjectID,
) (*models.Crawling, error) {
	var crawling models.Crawling

	err := r.crawlings().FindOne(ctx,
		bson.M{
			"site_id": siteID,
			"_id":     bson.M{"$ne": excludeCrawlingID},
			"status":  models.CrawlStatusCompleted,
		},
		options.FindOne().SetSort(bson.D{{Key: "completed_at", Value: -1}}),
	).Decode(&crawling)

	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}

	return &crawling, nil
}

// SaveAlerts stores the alerts detected for one round.
//
// Insert is idempotent per round: detection re-running for a crawling that
// already has alerts deletes the old ones first, so a retried completion
// cannot double-report a regression to the customer.
func (r *MongoRepository) SaveAlerts(
	ctx context.Context,
	siteID, crawlingID primitive.ObjectID,
	alerts []models.AlertEvent,
) error {
	if _, err := r.alertEvents().DeleteMany(ctx, bson.M{"crawling_id": crawlingID}); err != nil {
		return err
	}

	if len(alerts) == 0 {
		return nil
	}

	now := time.Now().UTC()
	docs := make([]interface{}, 0, len(alerts))

	for _, alert := range alerts {
		alert.ID = primitive.NilObjectID
		alert.SiteID = siteID
		alert.CrawlingID = crawlingID
		alert.CreatedAt = now
		docs = append(docs, alert)
	}

	_, err := r.alertEvents().InsertMany(ctx, docs)
	return err
}

// ListAlerts returns a site's alerts, newest first.
//
// Dismissed alerts are excluded by default: the list is a working inbox, and
// something already dealt with reappearing on every load is what makes people
// stop reading it.
func (r *MongoRepository) ListAlerts(
	ctx context.Context,
	siteID primitive.ObjectID,
	since time.Time,
	includeDismissed bool,
	limit int64,
) ([]models.AlertEvent, error) {
	filter := bson.M{
		"site_id":    siteID,
		"created_at": bson.M{"$gte": since},
	}
	if !includeDismissed {
		filter["dismissed_at"] = nil
	}

	cursor, err := r.alertEvents().Find(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "severity", Value: -1}}).
			SetLimit(limit),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	alerts := make([]models.AlertEvent, 0)
	if err := cursor.All(ctx, &alerts); err != nil {
		return nil, err
	}

	return alerts, nil
}

// DismissAlert marks one alert as dealt with. Returns false when no such alert
// exists, so the handler can answer 404 rather than pretending it worked.
func (r *MongoRepository) DismissAlert(ctx context.Context, alertID primitive.ObjectID) (bool, error) {
	res, err := r.alertEvents().UpdateOne(ctx,
		bson.M{"_id": alertID},
		bson.M{"$set": bson.M{"dismissed_at": time.Now().UTC()}},
	)
	if err != nil {
		return false, err
	}

	return res.MatchedCount > 0, nil
}

// AlertsForDelivery returns undismissed alerts across a set of sites created
// after a cutoff, which is what the dashboard's send commands ask for.
//
// Taking many site ids in one call keeps delivery from issuing a query per
// site — the fan-out pattern that took the dashboard down once already.
func (r *MongoRepository) AlertsForDelivery(
	ctx context.Context,
	siteIDs []primitive.ObjectID,
	since time.Time,
	minSeverity int,
	limit int64,
) ([]models.AlertEvent, error) {
	if len(siteIDs) == 0 {
		return []models.AlertEvent{}, nil
	}

	cursor, err := r.alertEvents().Find(ctx,
		bson.M{
			"site_id":      bson.M{"$in": siteIDs},
			"created_at":   bson.M{"$gt": since},
			"severity":     bson.M{"$gte": minSeverity},
			"dismissed_at": nil,
		},
		options.Find().
			SetSort(bson.D{{Key: "site_id", Value: 1}, {Key: "severity", Value: -1}}).
			SetLimit(limit),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	alerts := make([]models.AlertEvent, 0)
	if err := cursor.All(ctx, &alerts); err != nil {
		return nil, err
	}

	return alerts, nil
}

// PruneAlertsBefore deletes alerts older than a cutoff, so they age out with
// the crawl data they describe instead of accumulating forever the way
// error_logs did.
func (r *MongoRepository) PruneAlertsBefore(
	ctx context.Context,
	siteID primitive.ObjectID,
	before time.Time,
) (int64, error) {
	res, err := r.alertEvents().DeleteMany(ctx, bson.M{
		"site_id":    siteID,
		"created_at": bson.M{"$lt": before},
	})
	if err != nil {
		return 0, err
	}

	return res.DeletedCount, nil
}
