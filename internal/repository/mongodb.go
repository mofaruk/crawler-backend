package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/webkonsulenterne/crawler-backend/internal/config"
	"github.com/webkonsulenterne/crawler-backend/internal/models"
)

type MongoRepository struct {
	client *mongo.Client
	db     *mongo.Database
}

func NewMongoRepository(cfg *config.Config) (*MongoRepository, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := options.Client().
		ApplyURI(cfg.MongoURI).
		SetMaxPoolSize(uint64(cfg.MongoPoolSz)).
		SetMinPoolSize(10).
		SetMaxConnIdleTime(30 * time.Second)

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	db := client.Database(cfg.MongoDB)
	repo := &MongoRepository{client: client, db: db}

	if err := repo.ensureIndexes(ctx); err != nil {
		log.Warn().Err(err).Msg("failed to create some indexes")
	}

	return repo, nil
}

func (r *MongoRepository) Close(ctx context.Context) error {
	return r.client.Disconnect(ctx)
}

// --- Index Setup ---

func (r *MongoRepository) ensureIndexes(ctx context.Context) error {
	// base_url is intentionally NOT unique: two different users may legitimately
	// crawl the same site, each owning their own site document. Per-user
	// uniqueness is enforced in the dashboard. Drop the legacy unique index if
	// a previous deploy created it (CreateMany won't replace a conflicting one).
	if _, err := r.sites().Indexes().DropOne(ctx, "base_url_1"); err != nil {
		// "index not found" is expected on fresh installs — log nothing.
		if !strings.Contains(err.Error(), "index not found") &&
			!strings.Contains(err.Error(), "IndexNotFound") {
			log.Warn().Err(err).Msg("could not drop legacy unique base_url index")
		}
	}

	// sites indexes
	_, err := r.sites().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "base_url", Value: 1}}}, // non-unique, for lookups
		{Keys: bson.D{{Key: "name", Value: 1}}},
	})
	if err != nil {
		return err
	}

	// crawlings indexes
	_, err = r.crawlings().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "site_id", Value: 1}, {Key: "status", Value: 1}}},
		// Serves the common "crawlings for a site, newest first" query
		// (GET /crawlings?site_id=…) fully from the index — match on site_id
		// and the created_at sort both come from this one index, so no
		// in-memory sort of the (unbounded, continuous) per-site history.
		{Keys: bson.D{{Key: "site_id", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "created_at", Value: -1}}},
	})
	if err != nil {
		return err
	}

	// site_urls indexes: the stored list is looked up per site, and rebuilt
	// by upsert on (site_id, url_hash).
	_, err = r.siteURLs().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "site_id", Value: 1}, {Key: "url_hash", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "site_id", Value: 1}, {Key: "kind", Value: 1}}},
	})
	if err != nil {
		return err
	}

	// crawl_urls indexes
	_, err = r.crawlURLs().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "crawling_id", Value: 1}, {Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "crawling_id", Value: 1}, {Key: "url_hash", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "url_hash", Value: 1}}},
	})
	if err != nil {
		return err
	}

	// crawling_results indexes
	_, err = r.crawlingResults().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "crawling_id", Value: 1}}},
		// Covers ListCrawlingResultsByCursor, which filters on crawling_id and
		// sorts _id descending. Without it Mongo sorts in memory and fails
		// outright past the 32MB sort limit on a large crawl.
		{Keys: bson.D{{Key: "crawling_id", Value: 1}, {Key: "_id", Value: -1}}},
		{Keys: bson.D{{Key: "site_id", Value: 1}}},
		{Keys: bson.D{{Key: "crawled_at", Value: -1}}},
	})
	if err != nil {
		return err
	}

	// crawl_failures indexes
	_, err = r.crawlFailures().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "crawling_id", Value: 1}}},
		{Keys: bson.D{{Key: "failed_at", Value: -1}}},
	})
	if err != nil {
		return err
	}

	// site_timeline indexes. One point per round, read by site and plotted in
	// time order; the unique crawling_id is what makes a repeated roll-up
	// replace rather than duplicate.
	_, err = r.siteTimeline().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "site_id", Value: 1}, {Key: "crawled_at", Value: 1}}},
		{Keys: bson.D{{Key: "crawling_id", Value: 1}}, Options: options.Index().SetUnique(true)},
	})
	if err != nil {
		return err
	}

	// alert_events indexes. Listing a site's alerts and replacing one round's
	// alerts are the only two access patterns, and both are on the hot path of
	// finishing a crawl.
	_, err = r.alertEvents().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "site_id", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "crawling_id", Value: 1}}},
	})

	return err
}

// --- Collection Accessors ---

func (r *MongoRepository) sites() *mongo.Collection     { return r.db.Collection("sites") }
func (r *MongoRepository) crawlings() *mongo.Collection { return r.db.Collection("crawlings") }
func (r *MongoRepository) crawlURLs() *mongo.Collection { return r.db.Collection("crawl_urls") }
func (r *MongoRepository) crawlingResults() *mongo.Collection {
	return r.db.Collection("crawling_results")
}
func (r *MongoRepository) crawlFailures() *mongo.Collection { return r.db.Collection("crawl_failures") }

// --- Site Operations ---

func (r *MongoRepository) CreateSite(ctx context.Context, site *models.Site) error {
	site.CreatedAt = time.Now()
	site.UpdatedAt = time.Now()
	result, err := r.sites().InsertOne(ctx, site)
	if err != nil {
		return err
	}
	site.ID = result.InsertedID.(primitive.ObjectID)
	return nil
}

func (r *MongoRepository) GetSite(ctx context.Context, id primitive.ObjectID) (*models.Site, error) {
	var site models.Site
	err := r.sites().FindOne(ctx, bson.M{"_id": id}).Decode(&site)
	if err != nil {
		return nil, err
	}
	return &site, nil
}

func (r *MongoRepository) ListSites(ctx context.Context, skip, limit int64) ([]models.Site, int64, error) {
	total, err := r.sites().CountDocuments(ctx, bson.M{})
	if err != nil {
		return nil, 0, err
	}

	opts := options.Find().SetSkip(skip).SetLimit(limit).SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := r.sites().Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cursor.Close(ctx)

	var sites []models.Site
	if err := cursor.All(ctx, &sites); err != nil {
		return nil, 0, err
	}
	return sites, total, nil
}

func (r *MongoRepository) UpdateSite(ctx context.Context, id primitive.ObjectID, update bson.M) (*models.Site, error) {
	update["updated_at"] = time.Now()
	result := r.sites().FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": update},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	var site models.Site
	if err := result.Decode(&site); err != nil {
		return nil, err
	}
	return &site, nil
}

// DeleteSite removes a site and every record belonging to it.
//
// Deleting only the site document orphaned its crawlings, results, URLs and
// failures: nothing references them afterwards, no endpoint can reach them,
// and they are never reclaimed. One deletion in local testing stranded 2,755
// result rows — 80% of the collection.
func (r *MongoRepository) DeleteSite(ctx context.Context, id primitive.ObjectID) error {
	filter := bson.M{"site_id": id}
	if _, err := r.crawlingResults().DeleteMany(ctx, filter); err != nil {
		return err
	}
	if _, err := r.crawlURLs().DeleteMany(ctx, filter); err != nil {
		return err
	}
	if _, err := r.crawlFailures().DeleteMany(ctx, filter); err != nil {
		return err
	}
	if _, err := r.crawlings().DeleteMany(ctx, filter); err != nil {
		return err
	}

	_, err := r.sites().DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// --- Crawling Operations ---

func (r *MongoRepository) CreateCrawling(ctx context.Context, crawling *models.Crawling) error {
	crawling.CreatedAt = time.Now()
	crawling.UpdatedAt = time.Now()
	result, err := r.crawlings().InsertOne(ctx, crawling)
	if err != nil {
		return err
	}
	crawling.ID = result.InsertedID.(primitive.ObjectID)
	return nil
}

// ActiveCrawlingForSite returns the site's in-flight crawl, or nil.
//
// A second concurrent crawl doubles the request rate against the customer's
// server — two rounds at 14,400/hr is 28,800/hr arriving at an origin that
// was sized for one. Callers use this to refuse the second start.
func (r *MongoRepository) ActiveCrawlingForSite(ctx context.Context, siteID primitive.ObjectID) (*models.Crawling, error) {
	var crawling models.Crawling

	err := r.crawlings().FindOne(ctx, bson.M{
		"site_id": siteID,
		"status": bson.M{"$in": bson.A{
			models.CrawlStatusPending,
			models.CrawlStatusDiscovering,
			models.CrawlStatusRunning,
			models.CrawlStatusPaused,
		}},
	}).Decode(&crawling)

	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &crawling, nil
}

func (r *MongoRepository) GetCrawling(ctx context.Context, id primitive.ObjectID) (*models.Crawling, error) {
	var crawling models.Crawling
	err := r.crawlings().FindOne(ctx, bson.M{"_id": id}).Decode(&crawling)
	if err != nil {
		return nil, err
	}
	return &crawling, nil
}

func (r *MongoRepository) UpdateCrawlingStatus(ctx context.Context, id primitive.ObjectID, status models.CrawlStatus) error {
	update := bson.M{
		"$set": bson.M{
			"status":     status,
			"updated_at": time.Now(),
		},
	}

	now := time.Now()
	switch status {
	case models.CrawlStatusDiscovering, models.CrawlStatusRunning:
		// Stamp started_at on the first transition out of pending. Use $setOnInsert
		// semantics via a separate update to avoid overwriting the original time
		// when status flips discovering → running mid-crawl.
		_, _ = r.crawlings().UpdateOne(ctx,
			bson.M{"_id": id, "started_at": bson.M{"$exists": false}},
			bson.M{"$set": bson.M{"started_at": now}},
		)
	case models.CrawlStatusCompleted, models.CrawlStatusFailed, models.CrawlStatusStopped:
		update["$set"].(bson.M)["completed_at"] = now
	case models.CrawlStatusPaused:
		update["$set"].(bson.M)["paused_at"] = now
	}

	_, err := r.crawlings().UpdateByID(ctx, id, update)
	return err
}

func (r *MongoRepository) UpdateCrawlingProgress(ctx context.Context, id primitive.ObjectID, crawled, failed int) error {
	_, err := r.crawlings().UpdateByID(ctx, id, bson.M{
		"$inc": bson.M{
			"crawled_urls": crawled,
			"failed_urls":  failed,
		},
		"$set": bson.M{"updated_at": time.Now()},
	})
	return err
}

func (r *MongoRepository) SetCrawlingTotalURLs(ctx context.Context, id primitive.ObjectID, total int) error {
	_, err := r.crawlings().UpdateByID(ctx, id, bson.M{
		"$set": bson.M{
			"total_urls": total,
			"updated_at": time.Now(),
		},
	})
	return err
}

// SetCrawlingCrawledURLs sets the crawled count directly. Used when a round
// finishes without fetching anything — every URL was carried forward — so
// there are no per-URL increments to arrive at the right figure.
func (r *MongoRepository) SetCrawlingCrawledURLs(ctx context.Context, id primitive.ObjectID, n int) error {
	now := time.Now()
	_, err := r.crawlings().UpdateByID(ctx, id, bson.M{
		"$set": bson.M{
			"crawled_urls": n,
			"completed_at": now,
			"updated_at":   now,
		},
	})
	return err
}

// IncCrawlingTotalURLs atomically increases total_urls by delta. Used during
// streaming auto-discovery, where the final URL count is unknown upfront.
func (r *MongoRepository) IncCrawlingTotalURLs(ctx context.Context, id primitive.ObjectID, delta int) error {
	if delta == 0 {
		return nil
	}
	_, err := r.crawlings().UpdateByID(ctx, id, bson.M{
		"$inc": bson.M{"total_urls": delta},
		"$set": bson.M{"updated_at": time.Now()},
	})
	return err
}

// CountUncacheablePages counts pages in a round the origin forbids the CDN to
// store. Assets are excluded: they follow their own caching rules and are not
// what the limit is about.
func (r *MongoRepository) CountUncacheablePages(ctx context.Context, crawlingID primitive.ObjectID) (int, error) {
	n, err := r.crawlingResults().CountDocuments(ctx, bson.M{
		"crawling_id":  crawlingID,
		"cannot_cache": true,
		// Pages only. An asset row has no page signals, so this is the same
		// distinction the rest of the reporting draws.
		"page": bson.M{"$ne": nil},
	})

	return int(n), err
}

// SetCrawlingStoppedReason records why a round ended before it ran out of URLs,
// so the dashboard can explain a short round rather than showing an
// unexplained stop.
func (r *MongoRepository) SetCrawlingStoppedReason(ctx context.Context, id primitive.ObjectID, reason string) error {
	_, err := r.crawlings().UpdateByID(ctx, id, bson.M{
		"$set": bson.M{"stopped_reason": reason},
	})

	return err
}

func (r *MongoRepository) SetCrawlingError(ctx context.Context, id primitive.ObjectID, errMsg string) error {
	_, err := r.crawlings().UpdateByID(ctx, id, bson.M{
		"$set": bson.M{
			"status":        models.CrawlStatusFailed,
			"error_message": errMsg,
			"updated_at":    time.Now(),
		},
	})
	return err
}

func (r *MongoRepository) ListCrawlings(ctx context.Context, filter bson.M, skip, limit int64) ([]models.Crawling, int64, error) {
	// Total count. An empty filter means "all crawlings": use the O(1) metadata
	// count rather than CountDocuments, which scans the whole collection and so
	// scales with its (unbounded, continuous-crawling) size. CountDocuments is
	// still used for filtered queries, where it rides the matching index.
	var (
		total int64
		err   error
	)
	if len(filter) == 0 {
		total, err = r.crawlings().EstimatedDocumentCount(ctx)
	} else {
		total, err = r.crawlings().CountDocuments(ctx, filter)
	}
	if err != nil {
		return nil, 0, err
	}

	opts := options.Find().SetSkip(skip).SetLimit(limit).SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := r.crawlings().Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cursor.Close(ctx)

	var crawlings []models.Crawling
	if err := cursor.All(ctx, &crawlings); err != nil {
		return nil, 0, err
	}
	return crawlings, total, nil
}

// --- Crawl URL Operations ---

func (r *MongoRepository) BulkInsertCrawlURLs(ctx context.Context, urls []models.CrawlURL) (int, error) {
	if len(urls) == 0 {
		return 0, nil
	}

	docs := make([]interface{}, len(urls))
	for i := range urls {
		urls[i].CreatedAt = time.Now()
		urls[i].UpdatedAt = time.Now()
		docs[i] = urls[i]
	}

	opts := options.InsertMany().SetOrdered(false) // skip duplicates
	result, err := r.crawlURLs().InsertMany(ctx, docs, opts)
	if err != nil {
		// Partial insert is OK (duplicates skipped)
		if mongo.IsDuplicateKeyError(err) {
			if result != nil {
				return len(result.InsertedIDs), nil
			}
			return 0, nil
		}
		return 0, err
	}
	return len(result.InsertedIDs), nil
}

func (r *MongoRepository) GetCrawlURLCountByStatus(ctx context.Context, crawlingID primitive.ObjectID, status models.URLStatus) (int64, error) {
	return r.crawlURLs().CountDocuments(ctx, bson.M{
		"crawling_id": crawlingID,
		"status":      status,
	})
}

// --- Result Operations ---

func (r *MongoRepository) InsertCrawlingResult(ctx context.Context, result *models.CrawlingResult) error {
	_, err := r.crawlingResults().InsertOne(ctx, result)
	return err
}

func (r *MongoRepository) BulkInsertResults(ctx context.Context, results []models.CrawlingResult) error {
	if len(results) == 0 {
		return nil
	}
	docs := make([]interface{}, len(results))
	for i := range results {
		docs[i] = results[i]
	}
	_, err := r.crawlingResults().InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
	return err
}

// --- Result Query Operations ---

// aggregateDistribution groups crawling_results by `$<groupField>` over the
// given match filter and returns the value→count distribution (count desc)
// plus the grand total. Shared by every analytics endpoint (per-crawl and
// per-site, header and status).
func (r *MongoRepository) aggregateDistribution(ctx context.Context, match bson.M, groupField string) ([]models.HeaderValueCount, int64, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$group", Value: bson.M{
			"_id":   "$" + groupField,
			"count": bson.M{"$sum": 1},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "count", Value: -1}}}},
	}

	cursor, err := r.crawlingResults().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, 0, err
	}
	defer cursor.Close(ctx)

	results := make([]models.HeaderValueCount, 0)
	var total int64
	for cursor.Next(ctx) {
		var doc struct {
			Value interface{} `bson:"_id"`
			Count int64       `bson:"count"`
		}
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		value := ""
		if doc.Value != nil {
			value = fmt.Sprintf("%v", doc.Value)
		}
		results = append(results, models.HeaderValueCount{Value: value, Count: doc.Count})
		total += doc.Count
	}
	return results, total, cursor.Err()
}

// GetHeaderAnalytics — per-crawl distribution of one response header's values.
func (r *MongoRepository) GetHeaderAnalytics(ctx context.Context, crawlingID primitive.ObjectID, headerName string) ([]models.HeaderValueCount, int64, error) {
	fieldPath := "headers." + headerName
	return r.aggregateDistribution(ctx, bson.M{
		"crawling_id": crawlingID,
		fieldPath:     bson.M{"$exists": true, "$ne": nil},
	}, fieldPath)
}

// GetCrawlingStatusAnalytics — per-crawl distribution of HTTP status codes.
func (r *MongoRepository) GetCrawlingStatusAnalytics(ctx context.Context, crawlingID primitive.ObjectID) ([]models.HeaderValueCount, int64, error) {
	return r.aggregateDistribution(ctx, bson.M{"crawling_id": crawlingID}, "status_code")
}

// GetSiteHeaderAnalytics — per-site distribution of one header's values across
// every result crawled in [from, to).
func (r *MongoRepository) GetSiteHeaderAnalytics(ctx context.Context, siteID primitive.ObjectID, headerName string, from, to time.Time) ([]models.HeaderValueCount, int64, error) {
	fieldPath := "headers." + headerName
	return r.aggregateDistribution(ctx, bson.M{
		"site_id":    siteID,
		"crawled_at": bson.M{"$gte": from, "$lt": to},
		fieldPath:    bson.M{"$exists": true, "$ne": nil},
	}, fieldPath)
}

// GetSiteStatusAnalytics — per-site HTTP status distribution across [from, to).
func (r *MongoRepository) GetSiteStatusAnalytics(ctx context.Context, siteID primitive.ObjectID, from, to time.Time) ([]models.HeaderValueCount, int64, error) {
	return r.aggregateDistribution(ctx, bson.M{
		"site_id":    siteID,
		"crawled_at": bson.M{"$gte": from, "$lt": to},
	}, "status_code")
}

// GetSiteTimeline returns one datapoint per completed crawl of a site, so the
// UI can plot how a site changed over time rather than only its current state.
//
// Everything here comes from stored results — no extra crawling — and each
// series answers a different question:
//
//	· cache coverage      is the CDN doing its job
//	· median age          how stale is what visitors receive
//	· response time       is the origin (or edge) getting slower
//	· errors              is the site breaking
//
// Plotted together they separate causes that a single number conflates: cache
// falling while response time rises is a CDN problem; both steady while errors
// climb is an origin problem.
func (r *MongoRepository) GetSiteTimeline(ctx context.Context, siteID primitive.ObjectID, since time.Time, limit int64) ([]models.TimelinePoint, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"site_id":    siteID,
			"crawled_at": bson.M{"$gte": since},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":        "$crawling_id",
			"crawled_at": bson.M{"$min": "$crawled_at"},
			"total":      bson.M{"$sum": 1},
			"errors": bson.M{"$sum": bson.M{
				"$cond": bson.A{bson.M{"$gte": bson.A{"$status_code", 400}}, 1, 0},
			}},
			"avg_response": bson.M{"$avg": "$response_time_ms"},
			// Ages arrive as header strings; convert what parses and ignore
			// the rest rather than dropping the whole datapoint.
			"ages": bson.M{"$push": bson.M{
				"$convert": bson.M{
					"input":   "$headers.Age",
					"to":      "double",
					"onError": nil,
					"onNull":  nil,
				},
			}},
			"cached": bson.M{"$sum": bson.M{
				"$cond": bson.A{
					bson.M{"$in": bson.A{"$headers.CF-Cache-Status", bson.A{"HIT", "REVALIDATED", "UPDATING"}}},
					1, 0,
				},
			}},
			"cache_known": bson.M{"$sum": bson.M{
				"$cond": bson.A{bson.M{"$ifNull": bson.A{"$headers.CF-Cache-Status", false}}, 1, 0},
			}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "crawled_at", Value: 1}}}},
		{{Key: "$limit", Value: limit}},
	}

	cursor, err := r.crawlingResults().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var raw []struct {
		CrawlingID  primitive.ObjectID `bson:"_id"`
		CrawledAt   time.Time          `bson:"crawled_at"`
		Total       int                `bson:"total"`
		Errors      int                `bson:"errors"`
		AvgResponse float64            `bson:"avg_response"`
		Ages        []*float64         `bson:"ages"`
		Cached      int                `bson:"cached"`
		CacheKnown  int                `bson:"cache_known"`
	}
	if err := cursor.All(ctx, &raw); err != nil {
		return nil, err
	}

	points := make([]models.TimelinePoint, 0, len(raw))
	for _, p := range raw {
		point := models.TimelinePoint{
			CrawlingID:    p.CrawlingID.Hex(),
			CrawledAt:     p.CrawledAt,
			URLs:          p.Total,
			Errors:        p.Errors,
			AvgResponseMs: int64(p.AvgResponse),
		}

		if p.CacheKnown > 0 {
			point.CachePercent = float64(int(float64(p.Cached)/float64(p.CacheKnown)*1000)) / 10
		}

		// Median rather than mean: one page cached for a year would drag an
		// average far away from what a typical visitor receives.
		var ages []float64
		for _, a := range p.Ages {
			if a != nil {
				ages = append(ages, *a)
			}
		}
		if len(ages) > 0 {
			sort.Float64s(ages)
			point.MedianAgeSeconds = int64(ages[len(ages)/2])
		}

		points = append(points, point)
	}

	return points, nil
}

// GetSiteIssues returns everything currently wrong with a site.
//
// "Currently" is the important part: the pipeline keeps only the newest result
// per URL *before* classifying, so a URL that 404'd last week but returns 200
// today is not reported. first_seen and occurrences come along so the caller
// can tell a transient blip from something broken for a fortnight.
//
// Detection is deliberately broad — every signal here is already stored, so
// reporting it costs one aggregation rather than another crawl.
func (r *MongoRepository) GetSiteIssues(ctx context.Context, siteID primitive.ObjectID, since time.Time, limit int64) ([]models.SiteIssue, error) {
	return r.GetSiteIssuesBetween(ctx, siteID, since, time.Now().UTC(), limit)
}

// GetSiteIssuesBetween is GetSiteIssues over an explicit window, so callers
// can ask about a specific date or range rather than only "the last N days".
func (r *MongoRepository) GetSiteIssuesBetween(ctx context.Context, siteID primitive.ObjectID, since, until time.Time, limit int64) ([]models.SiteIssue, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"site_id":    siteID,
			"crawled_at": bson.M{"$gte": since, "$lt": until},
		}}},
		// Newest first so $first below picks the current state of each URL.
		{{Key: "$sort", Value: bson.D{{Key: "crawled_at", Value: -1}}}},
		{{Key: "$group", Value: bson.M{
			"_id":           "$url",
			"status_code":   bson.M{"$first": "$status_code"},
			"content_type":  bson.M{"$first": "$content_type"},
			"response_time": bson.M{"$first": "$response_time_ms"},
			"redirected_to": bson.M{"$first": "$redirected_to"},
			"headers":       bson.M{"$first": "$headers"},
			"page":          bson.M{"$first": "$page"},
			"last_seen":     bson.M{"$first": "$crawled_at"},
			"first_seen":    bson.M{"$last": "$crawled_at"},
			"occurrences":   bson.M{"$sum": 1},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "status_code", Value: -1}}}},
		{{Key: "$project", Value: bson.M{
			"_id":           0,
			"url":           "$_id",
			"status_code":   1,
			"content_type":  1,
			"response_time": 1,
			"redirected_to": 1,
			"headers":       1,
			"page":          1,
			"first_seen":    1,
			"last_seen":     1,
			"occurrences":   1,
		}}},
	}

	cursor, err := r.crawlingResults().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var rows []models.URLState
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, err
	}

	// Duplicate titles can only be found across the whole set, so count them
	// before classifying any single URL.
	titleCounts := map[string]int{}
	for _, row := range rows {
		if row.Page != nil && row.Page.Title != "" {
			titleCounts[strings.ToLower(row.Page.Title)]++
		}
	}

	issues := make([]models.SiteIssue, 0, len(rows))
	for _, row := range rows {
		issues = append(issues, models.ClassifyURL(row, titleCounts)...)
	}

	// Most severe first, then most persistent.
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity > issues[j].Severity
		}
		return issues[i].Occurrences > issues[j].Occurrences
	})

	if int64(len(issues)) > limit {
		issues = issues[:limit]
	}
	return issues, nil
}

func (r *MongoRepository) GetCrawlingResults(ctx context.Context, crawlingID primitive.ObjectID, filter bson.M, skip, limit int64) ([]models.CrawlingResult, int64, error) {
	filter["crawling_id"] = crawlingID

	total, err := r.crawlingResults().CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	opts := options.Find().SetSkip(skip).SetLimit(limit).SetSort(bson.D{{Key: "crawled_at", Value: -1}})
	cursor, err := r.crawlingResults().Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cursor.Close(ctx)

	var results []models.CrawlingResult
	if err := cursor.All(ctx, &results); err != nil {
		return nil, 0, err
	}
	return results, total, nil
}

// ListCrawlingResultsByCursor returns results sorted by _id descending. If
// cursor is non-zero, only returns documents with _id < cursor. Returns at
// most limit results plus a hasMore flag (the caller is responsible for using
// the last result's ID as the next cursor).
func (r *MongoRepository) ListCrawlingResultsByCursor(
	ctx context.Context,
	crawlingID primitive.ObjectID,
	filter bson.M,
	cursor primitive.ObjectID,
	limit int64,
) ([]models.CrawlingResult, bool, error) {
	filter["crawling_id"] = crawlingID
	if !cursor.IsZero() {
		filter["_id"] = bson.M{"$lt": cursor}
	}

	// Fetch one extra document to detect has-more without a separate count.
	opts := options.Find().
		SetLimit(limit + 1).
		SetSort(bson.D{{Key: "_id", Value: -1}})

	cur, err := r.crawlingResults().Find(ctx, filter, opts)
	if err != nil {
		return nil, false, err
	}
	defer cur.Close(ctx)

	var results []models.CrawlingResult
	if err := cur.All(ctx, &results); err != nil {
		return nil, false, err
	}

	hasMore := int64(len(results)) > limit
	if hasMore {
		results = results[:limit]
	}
	return results, hasMore, nil
}

// StreamCrawlingResults yields each matching result to the callback in
// _id-desc order. Used for CSV export to avoid loading the full set into
// memory. The callback returning an error stops the iteration.
func (r *MongoRepository) StreamCrawlingResults(
	ctx context.Context,
	crawlingID primitive.ObjectID,
	filter bson.M,
	yield func(*models.CrawlingResult) error,
) error {
	filter["crawling_id"] = crawlingID

	opts := options.Find().
		SetSort(bson.D{{Key: "_id", Value: -1}}).
		SetBatchSize(500)

	cur, err := r.crawlingResults().Find(ctx, filter, opts)
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var doc models.CrawlingResult
		if err := cur.Decode(&doc); err != nil {
			return err
		}
		if err := yield(&doc); err != nil {
			return err
		}
	}
	return cur.Err()
}

// --- Failure Operations ---

func (r *MongoRepository) InsertCrawlFailure(ctx context.Context, failure *models.CrawlFailure) error {
	_, err := r.crawlFailures().InsertOne(ctx, failure)
	return err
}

func (r *MongoRepository) GetCrawlFailures(ctx context.Context, crawlingID primitive.ObjectID, skip, limit int64) ([]models.CrawlFailure, error) {
	opts := options.Find().SetSkip(skip).SetLimit(limit).SetSort(bson.D{{Key: "failed_at", Value: -1}})
	cursor, err := r.crawlFailures().Find(ctx, bson.M{"crawling_id": crawlingID}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var failures []models.CrawlFailure
	if err := cursor.All(ctx, &failures); err != nil {
		return nil, err
	}
	return failures, nil
}

// --- Cleanup ---

func (r *MongoRepository) DeleteCrawlData(ctx context.Context, crawlingID primitive.ObjectID) error {
	filter := bson.M{"crawling_id": crawlingID}
	if _, err := r.crawlURLs().DeleteMany(ctx, filter); err != nil {
		return err
	}
	if _, err := r.crawlingResults().DeleteMany(ctx, filter); err != nil {
		return err
	}
	if _, err := r.crawlFailures().DeleteMany(ctx, filter); err != nil {
		return err
	}
	return nil
}

// PruneCrawlingsBefore deletes a site's finished crawl rounds created before
// `before`, cascading to their crawl_urls, crawling_results and crawl_failures.
// Only terminal rounds (completed/stopped/failed) are eligible — an in-flight
// round (pending/discovering/running/paused) is never touched, even if old.
// Returns the number of crawlings deleted. Drives package-based data retention.
func (r *MongoRepository) PruneCrawlingsBefore(ctx context.Context, siteID primitive.ObjectID, before time.Time) (int64, error) {
	filter := bson.M{
		"site_id":    siteID,
		"created_at": bson.M{"$lt": before},
		"status": bson.M{"$in": bson.A{
			models.CrawlStatusCompleted,
			models.CrawlStatusStopped,
			models.CrawlStatusFailed,
		}},
	}

	// Collect the matching crawling ids (only _id needed).
	cursor, err := r.crawlings().Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return 0, err
	}
	var docs []struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := cursor.All(ctx, &docs); err != nil {
		return 0, err
	}
	if len(docs) == 0 {
		return 0, nil
	}

	ids := make([]primitive.ObjectID, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}

	// Cascade-delete dependent data first (rides the crawling_id index on each
	// collection), then the crawlings themselves.
	child := bson.M{"crawling_id": bson.M{"$in": ids}}
	if _, err := r.crawlURLs().DeleteMany(ctx, child); err != nil {
		return 0, err
	}
	if _, err := r.crawlingResults().DeleteMany(ctx, child); err != nil {
		return 0, err
	}
	if _, err := r.crawlFailures().DeleteMany(ctx, child); err != nil {
		return 0, err
	}

	res, err := r.crawlings().DeleteMany(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

// CachedResultsFromLastCrawl returns the previous completed round's results
// for URLs that were served from cache, keyed by URL.
//
// Used by smart recrawl: those URLs are skipped this round and their result
// copied forward, so the reported cache percentage still covers the whole
// site rather than only the handful of URLs that were re-fetched.
//
// Only a HIT-family status counts. A MISS or BYPASS last time is exactly the
// URL worth checking again, and an absent CF-Cache-Status means the CDN is
// not answering for it at all.
//
// maxAge bounds how long a result may be reused. A result is only reusable
// while its *original* fetch is within the window — carrying a row forward
// refreshes crawled_at every round, so measuring against that would let a
// result be renewed indefinitely and never re-fetched.
func (r *MongoRepository) CachedResultsFromLastCrawl(
	ctx context.Context,
	siteID primitive.ObjectID,
	excludeCrawlingID primitive.ObjectID,
	maxAge time.Duration,
) (map[string]models.CrawlingResult, error) {
	var last models.Crawling
	err := r.crawlings().FindOne(ctx,
		bson.M{
			"site_id": siteID,
			"_id":     bson.M{"$ne": excludeCrawlingID},
			"status":  models.CrawlStatusCompleted,
		},
		options.FindOne().SetSort(bson.D{{Key: "completed_at", Value: -1}}),
	).Decode(&last)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// First crawl of this site: nothing to carry forward, crawl it all.
			return nil, nil
		}
		return nil, err
	}

	// The age to compare is when the URL was actually fetched: on a carried
	// row that is original_crawled_at, and on a freshly fetched one there is
	// no such field, so crawled_at is the real time. $ifNull picks whichever
	// applies rather than trusting crawled_at, which every carry-forward
	// resets to now.
	cutoff := time.Now().Add(-maxAge)

	// Two kinds of URL are worth carrying forward rather than re-fetching.
	//
	// A cached one, because that is the whole point of smart recrawl. And one
	// the origin forbids caching, because warming it can never succeed: the
	// page reports MISS on every round, so it would otherwise be re-fetched
	// forever, spending the customer's quota and hitting their origin to learn
	// something we already know. Those are marked cache_forbidden and reported
	// as an issue instead.
	//
	// The bounded window still applies to both, so a fixed cache policy is
	// picked up on the next round rather than staying skipped forever.
	cursor, err := r.crawlingResults().Find(ctx, bson.M{
		"crawling_id": last.ID,
		"$or": bson.A{
			bson.M{"headers.CF-Cache-Status": bson.M{
				"$in": bson.A{"HIT", "REVALIDATED", "UPDATING"},
			}},
			bson.M{"cannot_cache": true},
		},
		"$expr": bson.M{
			"$gte": bson.A{
				bson.M{"$ifNull": bson.A{"$original_crawled_at", "$crawled_at"}},
				cutoff,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	out := make(map[string]models.CrawlingResult)
	for cursor.Next(ctx) {
		var res models.CrawlingResult
		if err := cursor.Decode(&res); err != nil {
			return nil, err
		}
		out[res.URL] = res
	}

	return out, cursor.Err()
}

// CarryForwardResults copies previous-round results into the current crawl,
// marked so a stale row is never mistaken for a freshly fetched one.
func (r *MongoRepository) CarryForwardResults(
	ctx context.Context,
	crawlingID primitive.ObjectID,
	previous []models.CrawlingResult,
) error {
	if len(previous) == 0 {
		return nil
	}

	docs := make([]interface{}, 0, len(previous))
	now := time.Now()

	for _, res := range previous {
		original := res.CrawledAt
		if res.OriginalCrawledAt != nil {
			// Already carried once; keep pointing at the real fetch, not the
			// round that copied it, or the age would reset every crawl.
			original = *res.OriginalCrawledAt
		}

		res.ID = primitive.NilObjectID
		res.CrawlingID = crawlingID
		res.CrawledAt = now
		res.CarriedForward = true
		res.OriginalCrawledAt = &original

		docs = append(docs, res)
	}

	_, err := r.crawlingResults().InsertMany(ctx, docs)

	return err
}

// TailCrawlingResults returns results newer than the given id, oldest first.
//
// The live feed polls with the newest id it has already shown, so each call
// returns only what has landed since. Passing a zero cursor returns the most
// recent `limit` rows so a viewer arriving mid-crawl sees context rather than
// an empty panel.
//
// ObjectIDs carry a timestamp prefix and rise monotonically per process, so
// "_id greater than" is a usable stand-in for "inserted after" here without a
// separate sequence.
func (r *MongoRepository) TailCrawlingResults(
	ctx context.Context,
	crawlingID primitive.ObjectID,
	after primitive.ObjectID,
	limit int64,
) ([]models.CrawlingResult, error) {
	filter := bson.M{"crawling_id": crawlingID}

	if after.IsZero() {
		// First poll: hand back the newest rows, then reverse so the caller
		// still receives them oldest-first like every later poll.
		opts := options.Find().
			SetLimit(limit).
			SetSort(bson.D{{Key: "_id", Value: -1}})

		cur, err := r.crawlingResults().Find(ctx, filter, opts)
		if err != nil {
			return nil, err
		}
		defer cur.Close(ctx)

		var newest []models.CrawlingResult
		if err := cur.All(ctx, &newest); err != nil {
			return nil, err
		}

		for i, j := 0, len(newest)-1; i < j; i, j = i+1, j-1 {
			newest[i], newest[j] = newest[j], newest[i]
		}

		return newest, nil
	}

	filter["_id"] = bson.M{"$gt": after}

	opts := options.Find().
		SetLimit(limit).
		SetSort(bson.D{{Key: "_id", Value: 1}})

	cur, err := r.crawlingResults().Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var results []models.CrawlingResult
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}

	return results, nil
}

func (r *MongoRepository) outboundLinks() *mongo.Collection {
	return r.db.Collection("outbound_links")
}

// RecordOutboundLinks upserts the external destinations found on one page.
//
// Deduplicated per site: the same destination linked from many pages is one
// document, so the checker makes one request rather than one per page. FoundOn
// is capped because the count is what a report needs, not the full list of
// four hundred pages carrying the same footer link.
func (r *MongoRepository) RecordOutboundLinks(
	ctx context.Context,
	siteID primitive.ObjectID,
	links []models.OutboundLink,
) error {
	if len(links) == 0 {
		return nil
	}

	now := time.Now()
	models_ := make([]mongo.WriteModel, 0, len(links))

	for _, link := range links {
		// The crawler reports one page at a time; FoundOn on the stored
		// document accumulates across pages.
		pages := link.FoundOn
		if len(pages) == 0 {
			continue
		}

		filter := bson.M{"site_id": siteID, "url": link.URL}

		update := bson.M{
			"$set": bson.M{"last_seen_at": now},
			"$setOnInsert": bson.M{
				"site_id":       siteID,
				"url":           link.URL,
				"first_seen_at": now,
			},
			// $addToSet dedupes but has no $slice; $push slices but does not
			// dedupe. Dedup matters more here — the same page must not be
			// listed twice — and the cap is enforced by the count field plus a
			// bounded projection when reading.
			"$addToSet": bson.M{
				"found_on": bson.M{"$each": pages},
			},
		}

		models_ = append(models_,
			mongo.NewUpdateOneModel().SetFilter(filter).SetUpdate(update).SetUpsert(true))
	}

	// Unordered: one duplicate-key race must not abandon the rest of the batch.
	_, err := r.outboundLinks().BulkWrite(ctx, models_, options.BulkWrite().SetOrdered(false))

	return err
}

// OutboundLinksToCheck returns a site's external destinations that have not
// been checked since the given cutoff, oldest first.
//
// Checking is spread over time rather than done all at once: these are third
// parties' servers, and a site with thousands of outbound links should not
// arrive at any of them as a burst.
func (r *MongoRepository) OutboundLinksToCheck(
	ctx context.Context,
	siteID primitive.ObjectID,
	checkedBefore time.Time,
	limit int64,
) ([]models.OutboundLink, error) {
	filter := bson.M{
		"site_id": siteID,
		"$or": bson.A{
			bson.M{"checked_at": bson.M{"$exists": false}},
			bson.M{"checked_at": nil},
			bson.M{"checked_at": bson.M{"$lt": checkedBefore}},
		},
	}

	// found_on can grow long on a footer link; the checker only needs the URL,
	// so it is not fetched here.
	opts := options.Find().
		SetLimit(limit).
		SetSort(bson.D{{Key: "checked_at", Value: 1}}).
		SetProjection(bson.M{"found_on": 0})

	cur, err := r.outboundLinks().Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var links []models.OutboundLink
	if err := cur.All(ctx, &links); err != nil {
		return nil, err
	}

	return links, nil
}

// SaveLinkCheck stores the outcome of checking one destination.
func (r *MongoRepository) SaveLinkCheck(
	ctx context.Context,
	id primitive.ObjectID,
	statusCode int,
	checkErr string,
	responseTime int64,
) error {
	now := time.Now()

	_, err := r.outboundLinks().UpdateByID(ctx, id, bson.M{
		"$set": bson.M{
			"status_code":      statusCode,
			"error":            checkErr,
			"response_time_ms": responseTime,
			"checked_at":       now,
		},
	})

	return err
}

// BrokenOutboundLinks returns a site's destinations whose last check failed.
func (r *MongoRepository) BrokenOutboundLinks(
	ctx context.Context,
	siteID primitive.ObjectID,
	limit int64,
) ([]models.OutboundLink, error) {
	// Mirrors models.OutboundLink.Broken(): statuses that usually mean "we
	// block bots" (400, 403, 429, 451) are excluded, because social platforms
	// answer those to any non-browser request while serving people fine.
	filter := bson.M{
		"site_id":    siteID,
		"checked_at": bson.M{"$ne": nil},
		"$or": bson.A{
			bson.M{"error": bson.M{"$nin": bson.A{"", nil}}},
			bson.M{
				"status_code": bson.M{
					"$gte": 400,
					"$nin": bson.A{400, 403, 429, 451},
				},
			},
		},
	}

	// Cap the page list per link: a report needs a few examples, not the four
	// hundred pages carrying the same footer link.
	opts := options.Find().
		SetLimit(limit).
		SetSort(bson.D{{Key: "url", Value: 1}}).
		SetProjection(bson.M{"found_on": bson.M{"$slice": 20}})

	cur, err := r.outboundLinks().Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var links []models.OutboundLink
	if err := cur.All(ctx, &links); err != nil {
		return nil, err
	}

	return links, nil
}

func (r *MongoRepository) siteURLs() *mongo.Collection {
	return r.db.Collection("site_urls")
}

// RecordSiteURLs adds URLs to a site's stored list.
//
// Upserted rather than inserted so a rebuild refreshes last_seen_at instead of
// duplicating, and so several crawl workers can contribute concurrently
// without racing.
func (r *MongoRepository) RecordSiteURLs(
	ctx context.Context,
	siteID primitive.ObjectID,
	urls []models.SiteURL,
) error {
	if len(urls) == 0 {
		return nil
	}

	now := time.Now()
	writes := make([]mongo.WriteModel, 0, len(urls))

	for _, u := range urls {
		writes = append(writes, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"site_id": siteID, "url_hash": u.URLHash}).
			SetUpdate(bson.M{
				"$set": bson.M{"last_seen_at": now, "kind": u.Kind},
				"$setOnInsert": bson.M{
					"site_id":       siteID,
					"url":           u.URL,
					"url_hash":      u.URLHash,
					"first_seen_at": now,
				},
			}).
			SetUpsert(true))
	}

	// Unordered: one duplicate-key race must not abandon the rest of the batch.
	_, err := r.siteURLs().BulkWrite(ctx, writes, options.BulkWrite().SetOrdered(false))

	return err
}

// SiteURLList returns a site's stored URLs, pages first.
//
// Pages lead so that a crawl which runs out of budget has still warmed the
// pages — an asset is only worth warming if the page referencing it is.
func (r *MongoRepository) SiteURLList(
	ctx context.Context,
	siteID primitive.ObjectID,
	limit int64,
) ([]models.SiteURL, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "kind", Value: 1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		opts.SetLimit(limit)
	}

	cur, err := r.siteURLs().Find(ctx, bson.M{"site_id": siteID}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var urls []models.SiteURL
	if err := cur.All(ctx, &urls); err != nil {
		return nil, err
	}

	return urls, nil
}

// CountSiteURLs reports how many URLs are stored for a site, split by kind.
func (r *MongoRepository) CountSiteURLs(ctx context.Context, siteID primitive.ObjectID) (pages, assets int64, err error) {
	pages, err = r.siteURLs().CountDocuments(ctx, bson.M{"site_id": siteID, "kind": models.SiteURLKindPage})
	if err != nil {
		return 0, 0, err
	}

	assets, err = r.siteURLs().CountDocuments(ctx, bson.M{"site_id": siteID, "kind": models.SiteURLKindAsset})
	if err != nil {
		return 0, 0, err
	}

	return pages, assets, nil
}

// ClearSiteURLs drops a site's stored list so the next crawl rebuilds it.
//
// Used by the refresh action: removing the rows is what makes the rebuild
// authoritative, since a URL that no longer exists on the site would otherwise
// linger forever on its last_seen_at.
func (r *MongoRepository) ClearSiteURLs(ctx context.Context, siteID primitive.ObjectID) error {
	_, err := r.siteURLs().DeleteMany(ctx, bson.M{"site_id": siteID})
	if err != nil {
		return err
	}

	// Clearing the timestamp too, so a crawl treats the list as never built
	// rather than as freshly built and empty.
	_, err = r.sites().UpdateByID(ctx, siteID, bson.M{"$unset": bson.M{"urls_built_at": ""}})

	return err
}

// MarkSiteURLsBuilt records that a site's list has just been rebuilt.
func (r *MongoRepository) MarkSiteURLsBuilt(ctx context.Context, siteID primitive.ObjectID) error {
	now := time.Now()

	_, err := r.sites().UpdateByID(ctx, siteID, bson.M{
		"$set": bson.M{"urls_built_at": now, "updated_at": now},
	})

	return err
}
