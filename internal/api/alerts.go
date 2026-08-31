package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webkonsulenterne/crawler-backend/internal/models"
	"github.com/webkonsulenterne/crawler-backend/internal/source"
	"github.com/webkonsulenterne/crawler-backend/internal/webhook"
)

// GET /sites/:id/alerts?days=&from=&to=&limit=&include_dismissed=
//
// What changed on a site, newest first. Alerts are produced when a round
// finishes; this only reads them.
func (h *Handler) ListSiteAlerts(c *gin.Context) {
	siteID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid site ID", Code: "INVALID_ID"})
		return
	}

	if _, err := h.repo.GetSite(c.Request.Context(), siteID); err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "site not found", Code: "NOT_FOUND"})
		return
	}

	since, _, days := resolveWindow(c, 30, 365)

	limit := int64(100)
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	includeDismissed := c.Query("include_dismissed") == "true"

	alerts, err := h.repo.ListAlerts(c.Request.Context(), siteID, since, includeDismissed, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list site alerts")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to list alerts"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"site_id": siteID.Hex(),
		"days":    days,
		"since":   since,
		"data":    alerts,
		"total":   len(alerts),
	})
}

// POST /alerts/:id/dismiss
//
// Marks one alert as dealt with so it stops appearing in the list and stops
// being delivered. Dismissing is not deleting: the record stays for history.
func (h *Handler) DismissAlert(c *gin.Context) {
	alertID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "invalid alert ID", Code: "INVALID_ID"})
		return
	}

	found, err := h.repo.DismissAlert(c.Request.Context(), alertID)
	if err != nil {
		log.Error().Err(err).Msg("failed to dismiss alert")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to dismiss alert"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: "alert not found", Code: "NOT_FOUND"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"dismissed": true, "id": alertID.Hex()})
}

// POST /alerts/delivery
//
// Undismissed alerts across many sites at once, for the dashboard's send
// commands. Taking every site in one call keeps delivery from issuing a query
// per site — the fan-out that took the dashboard past its gateway timeout.
//
// POST rather than GET because the site list is unbounded and would not fit a
// query string for a customer with hundreds of sites.
func (h *Handler) AlertsForDelivery(c *gin.Context) {
	var req struct {
		SiteIDs     []string `json:"site_ids" binding:"required"`
		Since       string   `json:"since"`
		MinSeverity int      `json:"min_severity"`
		Limit       int64    `json:"limit"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: "INVALID_REQUEST"})
		return
	}

	ids := make([]primitive.ObjectID, 0, len(req.SiteIDs))
	for _, raw := range req.SiteIDs {
		id, err := primitive.ObjectIDFromHex(raw)
		if err != nil {
			// Skip rather than reject the batch: one bad id in a list of two
			// hundred should not stop everyone else's alerts being delivered.
			log.Warn().Str("site_id", raw).Msg("alert delivery: skipping unparseable site id")
			continue
		}
		ids = append(ids, id)
	}

	since := time.Time{}
	if req.Since != "" {
		if parsed, err := time.Parse(time.RFC3339, req.Since); err == nil {
			since = parsed
		}
	}

	minSeverity := req.MinSeverity
	if minSeverity <= 0 {
		minSeverity = models.SeverityWarning
	}

	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 500
	}

	alerts, err := h.repo.AlertsForDelivery(c.Request.Context(), ids, since, minSeverity, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to collect alerts for delivery")
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "failed to collect alerts"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": alerts, "total": len(alerts)})
}

// POST /alerts/webhook
//
// Deliver a site's alerts to a customer-supplied endpoint, signed so the
// receiver can verify the request came from us.
//
// Sending lives here rather than in the dashboard because the dispatcher —
// with its HMAC signing, retries and backoff — already does, and a second
// implementation would be a second place for the signature to be wrong.
func (h *Handler) SendAlertWebhook(c *gin.Context) {
	var req struct {
		WebhookURL string                   `json:"webhook_url" binding:"required"`
		Secret     string                   `json:"secret"`
		SiteID     string                   `json:"site_id"`
		SiteURL    string                   `json:"site_url"`
		Alerts     []map[string]interface{} `json:"alerts" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: err.Error(), Code: "INVALID_REQUEST"})
		return
	}

	// The URL comes from a customer, and we fetch it from inside the network.
	// Without this the webhook field is an SSRF primitive pointed at whatever
	// the crawler can reach — the same reason base_url is validated.
	if err := source.ValidateTargetURL(req.WebhookURL, h.cfg.AllowPrivateTargets); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{
			Error: "webhook URL is not allowed: " + err.Error(),
			Code:  "INVALID_TARGET",
		})
		return
	}

	if len(req.Alerts) == 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "no alerts to send", Code: "INVALID_REQUEST"})
		return
	}

	payload := webhook.WebhookPayload{
		Event:     webhook.EventAlerts,
		Timestamp: time.Now().UTC(),
		Data: map[string]interface{}{
			"site_id":  req.SiteID,
			"site_url": req.SiteURL,
			"count":    len(req.Alerts),
			"alerts":   req.Alerts,
		},
	}

	if err := h.webhooks.Send(c.Request.Context(), req.WebhookURL, req.Secret, payload); err != nil {
		// The dispatcher has already retried with backoff, so this is a
		// settled failure rather than a transient one. 502: the request was
		// fine, the customer's endpoint is what did not work.
		log.Warn().Err(err).Str("site_id", req.SiteID).Msg("alert webhook delivery failed")
		c.JSON(http.StatusBadGateway, models.ErrorResponse{
			Error: "webhook delivery failed: " + err.Error(),
			Code:  "DELIVERY_FAILED",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"delivered": true, "count": len(req.Alerts)})
}
