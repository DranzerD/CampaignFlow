package tests

import (
	"testing"
	"time"

	"ad-service/internal/models"
)

// today is the reference "now" for the selection tests.
var today = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func candidate(id string, bid int64, created time.Time, mutate ...func(*models.Candidate)) models.Candidate {
	c := models.Candidate{
		AdID:             id,
		CampaignID:       "campaign-" + id,
		Title:            "ad " + id,
		BidCents:         bid,
		AdStatus:         models.StatusActive,
		CampaignStatus:   models.StatusActive,
		StartDate:        "2026-09-01",
		EndDate:          "2026-10-01",
		BudgetCents:      100000,
		DailyBudgetCents: 10000,
		CreatedAt:        created,
	}
	for _, m := range mutate {
		m(&c)
	}
	return c
}

func TestRankCandidatesPicksHighestBid(t *testing.T) {
	ranked := models.RankCandidates([]models.Candidate{
		candidate("low", 100, today),
		candidate("high", 300, today),
		candidate("mid", 200, today),
	}, today)

	if len(ranked) != 3 {
		t.Fatalf("expected 3 eligible candidates, got %d", len(ranked))
	}
	if ranked[0].AdID != "high" {
		t.Errorf("expected highest bid first, got %q", ranked[0].AdID)
	}
	if ranked[1].AdID != "mid" || ranked[2].AdID != "low" {
		t.Errorf("unexpected order: %q, %q", ranked[1].AdID, ranked[2].AdID)
	}
}

func TestRankCandidatesTieBreaksOnCreatedAt(t *testing.T) {
	older := today.Add(-48 * time.Hour)
	newer := today.Add(-1 * time.Hour)

	ranked := models.RankCandidates([]models.Candidate{
		candidate("newer", 150, newer),
		candidate("older", 150, older),
	}, today)

	if ranked[0].AdID != "older" {
		t.Errorf("expected the older ad to win the tie, got %q", ranked[0].AdID)
	}
}

func TestRankCandidatesExcludesPausedAd(t *testing.T) {
	ranked := models.RankCandidates([]models.Candidate{
		candidate("paused", 900, today, func(c *models.Candidate) {
			c.AdStatus = models.StatusPaused
		}),
		candidate("active", 100, today),
	}, today)

	if len(ranked) != 1 || ranked[0].AdID != "active" {
		t.Fatalf("paused ad should be excluded, got %+v", ranked)
	}
}

func TestRankCandidatesExcludesPausedCampaign(t *testing.T) {
	ranked := models.RankCandidates([]models.Candidate{
		candidate("paused-campaign", 900, today, func(c *models.Candidate) {
			c.CampaignStatus = models.StatusPaused
		}),
	}, today)

	if len(ranked) != 0 {
		t.Fatalf("ads of paused campaigns should be excluded, got %+v", ranked)
	}
}

func TestRankCandidatesExcludesCampaignsOutsideTheirDates(t *testing.T) {
	cands := []models.Candidate{
		candidate("expired", 900, today, func(c *models.Candidate) {
			c.StartDate, c.EndDate = "2026-08-01", "2026-09-22"
		}),
		candidate("not-started", 900, today, func(c *models.Candidate) {
			c.StartDate, c.EndDate = "2026-09-24", "2026-10-30"
		}),
		candidate("last-day", 10, today, func(c *models.Candidate) {
			c.StartDate, c.EndDate = "2026-09-01", "2026-09-23"
		}),
	}

	ranked := models.RankCandidates(cands, today)
	if len(ranked) != 1 || ranked[0].AdID != "last-day" {
		t.Fatalf("only the campaign whose window includes today is eligible, got %+v", ranked)
	}
}

func TestRankCandidatesOnEmptyInput(t *testing.T) {
	if got := models.RankCandidates(nil, today); len(got) != 0 {
		t.Fatalf("expected no candidates, got %+v", got)
	}
}
