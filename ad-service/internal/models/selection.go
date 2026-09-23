package models

import (
	"sort"
	"time"
)

// DateLayout is the layout used for every date crossing the API boundary.
const DateLayout = "2006-01-02"

// Candidate is one ad (joined with its campaign) that targets a keyword.
// Candidates are what gets cached in Redis per keyword, so the struct carries
// everything the ranking step needs.
type Candidate struct {
	AdID             string    `json:"ad_id"`
	CampaignID       string    `json:"campaign_id"`
	Title            string    `json:"title"`
	BidCents         int64     `json:"bid_cents"`
	AdStatus         string    `json:"ad_status"`
	CampaignStatus   string    `json:"campaign_status"`
	StartDate        string    `json:"start_date"`
	EndDate          string    `json:"end_date"`
	BudgetCents      int64     `json:"budget_cents"`
	DailyBudgetCents int64     `json:"daily_budget_cents"`
	CreatedAt        time.Time `json:"created_at"`
}

// RankCandidates drops candidates that are not eligible at time now and
// returns the rest ordered by the serving rules: highest bid first, older ad
// wins a tie. Budget checks are deliberately not done here -- they need an
// atomic Redis reservation and happen while walking this list.
func RankCandidates(cands []Candidate, now time.Time) []Candidate {
	today := now.UTC().Format(DateLayout)

	eligible := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.AdStatus != StatusActive || c.CampaignStatus != StatusActive {
			continue
		}
		// Dates are zero-padded ISO dates, so string comparison is
		// equivalent to calendar comparison.
		if today < c.StartDate || today > c.EndDate {
			continue
		}
		eligible = append(eligible, c)
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].BidCents != eligible[j].BidCents {
			return eligible[i].BidCents > eligible[j].BidCents
		}
		return eligible[i].CreatedAt.Before(eligible[j].CreatedAt)
	})
	return eligible
}
