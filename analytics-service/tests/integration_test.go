package tests

// Integration tests for the Analytics Service. They need the real PostgreSQL
// from docker compose and are skipped unless INTEGRATION_TEST=1.
// See run-tests.sh at the repository root.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"analytics-service/internal/db"
	"analytics-service/internal/handlers"
	"analytics-service/internal/models"
)

// eventDay is the timestamp every test event carries. It is a fixed past date
// so that test data never collides with a live demo run.
var eventDay = time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)

func mustTime(t *testing.T) time.Time {
	t.Helper()
	return eventDay
}

type harness struct {
	store *db.Store
	api   *handlers.API
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("INTEGRATION_TEST") != "1" {
		t.Skip("set INTEGRATION_TEST=1 to run integration tests")
	}
	ctx := context.Background()

	store, err := db.New(ctx, os.Getenv("ANALYTICS_DATABASE_URL"))
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	if err := store.Migrate(ctx, migrationsDir()); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	t.Cleanup(store.Close)

	return &harness{store: store, api: &handlers.API{Store: store}}
}

func migrationsDir() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}
	return "../migrations"
}

// record writes one event the way the consumer would.
func (h *harness) record(t *testing.T, eventID, campaignID, eventType string) {
	t.Helper()
	err := h.store.RecordEvent(context.Background(), models.Event{
		ID:         eventID,
		AdID:       uuid.NewString(),
		CampaignID: campaignID,
		EventType:  eventType,
		Timestamp:  eventDay,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
}

func (h *harness) stats(t *testing.T, campaignID string) (int64, int64) {
	t.Helper()
	impressions, clicks, err := h.store.Stats(context.Background(), campaignID,
		eventDay.Format(models.DateLayout))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return impressions, clicks
}

func TestImpressionAndClickAggregation(t *testing.T) {
	h := newHarness(t)
	campaignID := uuid.NewString()

	h.record(t, uuid.NewString(), campaignID, models.EventImpression)
	if impressions, clicks := h.stats(t, campaignID); impressions != 1 || clicks != 0 {
		t.Fatalf("after one impression: %d impressions, %d clicks", impressions, clicks)
	}

	h.record(t, uuid.NewString(), campaignID, models.EventClick)
	if impressions, clicks := h.stats(t, campaignID); impressions != 1 || clicks != 1 {
		t.Fatalf("after one click: %d impressions, %d clicks", impressions, clicks)
	}
}

func TestMultipleEventsAggregate(t *testing.T) {
	h := newHarness(t)
	campaignID := uuid.NewString()

	for i := 0; i < 10; i++ {
		h.record(t, uuid.NewString(), campaignID, models.EventImpression)
	}
	for i := 0; i < 3; i++ {
		h.record(t, uuid.NewString(), campaignID, models.EventClick)
	}

	impressions, clicks := h.stats(t, campaignID)
	if impressions != 10 || clicks != 3 {
		t.Errorf("expected 10 impressions and 3 clicks, got %d and %d", impressions, clicks)
	}
}

func TestDuplicateEventDoesNotDoubleCount(t *testing.T) {
	h := newHarness(t)
	campaignID := uuid.NewString()
	eventID := uuid.NewString()

	// The same event id redelivered by RabbitMQ must be counted once.
	h.record(t, eventID, campaignID, models.EventImpression)
	h.record(t, eventID, campaignID, models.EventImpression)
	h.record(t, eventID, campaignID, models.EventImpression)

	if impressions, _ := h.stats(t, campaignID); impressions != 1 {
		t.Errorf("expected 1 impression after 3 redeliveries, got %d", impressions)
	}
}

func TestReportEndpoint(t *testing.T) {
	h := newHarness(t)
	campaignID := uuid.NewString()

	// Exactly at the privacy threshold, with a 7-ish CTR.
	for i := 0; i < 100; i++ {
		h.record(t, uuid.NewString(), campaignID, models.EventImpression)
	}
	for i := 0; i < 7; i++ {
		h.record(t, uuid.NewString(), campaignID, models.EventClick)
	}

	report := h.report(t, campaignID)
	if report.Impressions == nil || *report.Impressions != 100 {
		t.Fatalf("impressions missing from report: %+v", report)
	}
	if report.Clicks == nil || *report.Clicks != 7 {
		t.Fatalf("clicks missing from report: %+v", report)
	}
	if report.CTR == nil || *report.CTR != 0.07 {
		t.Fatalf("expected ctr 0.07, got %+v", report.CTR)
	}
}

func TestReportSuppressesBelowThreshold(t *testing.T) {
	h := newHarness(t)
	campaignID := uuid.NewString()

	for i := 0; i < models.PrivacyThreshold-1; i++ {
		h.record(t, uuid.NewString(), campaignID, models.EventImpression)
	}
	h.record(t, uuid.NewString(), campaignID, models.EventClick)

	report := h.report(t, campaignID)
	if report.Status != "insufficient data" {
		t.Errorf("expected suppression, got %+v", report)
	}
	if report.Impressions != nil || report.Clicks != nil || report.CTR != nil {
		t.Errorf("suppressed report leaked numbers: %+v", report)
	}
}

func TestReportForUnknownCampaign(t *testing.T) {
	h := newHarness(t)
	// No events at all is simply "insufficient data", not an error.
	if report := h.report(t, uuid.NewString()); report.Status != "insufficient data" {
		t.Errorf("expected suppression for an unknown campaign, got %+v", report)
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.api.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health: %d", rec.Code)
	}
}

func (h *harness) report(t *testing.T, campaignID string) models.Report {
	t.Helper()
	url := "/reports/campaign/" + campaignID + "?date=" + eventDay.Format(models.DateLayout)
	rec := httptest.NewRecorder()
	h.api.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
	var report models.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report %q: %v", rec.Body.String(), err)
	}
	return report
}
