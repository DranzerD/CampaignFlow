package tests

import (
	"testing"

	"analytics-service/internal/models"
)

func TestCTR(t *testing.T) {
	cases := []struct {
		name                string
		impressions, clicks int64
		want                float64
	}{
		{"seven percent", 100, 7, 0.07},
		{"no clicks", 100, 0, 0},
		{"every impression clicked", 50, 50, 1},
		{"no impressions is not a division by zero", 0, 0, 0},
		{"rounded to four decimals", 3, 1, 0.3333},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := models.CTR(tc.impressions, tc.clicks); got != tc.want {
				t.Errorf("CTR(%d, %d) = %v, want %v", tc.impressions, tc.clicks, got, tc.want)
			}
		})
	}
}

func TestBuildReportSuppressesSmallAudiences(t *testing.T) {
	report := models.BuildReport("c1", "2026-09-23", models.PrivacyThreshold-1, 10)

	if report.Status != "insufficient data" {
		t.Errorf("expected the privacy status, got %q", report.Status)
	}
	if report.Impressions != nil || report.Clicks != nil || report.CTR != nil {
		t.Errorf("suppressed reports must not expose numbers: %+v", report)
	}
}

func TestBuildReportAtThreshold(t *testing.T) {
	report := models.BuildReport("c1", "2026-09-23", models.PrivacyThreshold, 5)

	if report.Status != "" {
		t.Errorf("expected full statistics at the threshold, got status %q", report.Status)
	}
	if report.Impressions == nil || *report.Impressions != models.PrivacyThreshold {
		t.Fatalf("impressions not reported: %+v", report.Impressions)
	}
	if report.Clicks == nil || *report.Clicks != 5 {
		t.Fatalf("clicks not reported: %+v", report.Clicks)
	}
	if report.CTR == nil || *report.CTR != 0.1 {
		t.Fatalf("expected ctr 0.1, got %+v", report.CTR)
	}
}

func TestEventValidation(t *testing.T) {
	valid := models.Event{
		ID:         "11111111-1111-1111-1111-111111111111",
		AdID:       "22222222-2222-2222-2222-222222222222",
		CampaignID: "33333333-3333-3333-3333-333333333333",
		EventType:  models.EventImpression,
		Timestamp:  mustTime(t),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	bad := valid
	bad.EventType = "conversion"
	if err := bad.Validate(); err == nil {
		t.Error("unknown event types must be rejected")
	}

	missing := valid
	missing.ID = ""
	if err := missing.Validate(); err == nil {
		t.Error("events without an id must be rejected")
	}

	// A non-empty but malformed id must be rejected too: it is not just an
	// empty-string check, since events_raw stores these as UUID columns and a
	// malformed value would otherwise fail the insert on every redelivery and
	// requeue forever.
	for _, field := range []string{"id", "ad_id", "campaign_id"} {
		malformed := valid
		switch field {
		case "id":
			malformed.ID = "not-a-uuid"
		case "ad_id":
			malformed.AdID = "not-a-uuid"
		case "campaign_id":
			malformed.CampaignID = "not-a-uuid"
		}
		if err := malformed.Validate(); err == nil {
			t.Errorf("malformed %s must be rejected", field)
		}
	}
}
