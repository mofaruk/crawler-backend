package repository

import (
	"context"
	"fmt"
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
		{Keys: bson.D{{Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "created_at", Value: -1}}},
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

	return err
}

// --- Collection Accessors ---

func (r *MongoRepository) sites() *mongo.Collection       { return r.db.Collection("sites") }
func (r *MongoRepository) crawlings() *mongo.Collection   { return r.db.Collection("crawlings") }
func (r *MongoRepository) crawlURLs() *mongo.Collection   { return r.db.Collection("crawl_urls") }
func (r *MongoRepository) crawlingResults() *mongo.Collection { return r.db.Collection("crawling_results") }
func (r *MongoRepository) crawlFailures() *mongo.Collection  { return r.db.Collection("crawl_failures") }

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

func (r *MongoRepository) DeleteSite(ctx context.Context, id primitive.ObjectID) error {
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
			"total_urls":  total,
			"updated_at":  time.Now(),
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
	total, err := r.crawlings().CountDocuments(ctx, filter)
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
