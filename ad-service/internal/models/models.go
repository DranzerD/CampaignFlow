package models

// StatusActive and StatusPaused are the only allowed campaign/ad statuses.
const (
	StatusActive = "active"
	StatusPaused = "paused"
)

// ValidStatus reports whether s is an allowed campaign/ad status.
func ValidStatus(s string) bool {
	return s == StatusActive || s == StatusPaused
}

// Campaign is the API representation of a campaign. Dates are plain
// "YYYY-MM-DD" strings so that JSON stays readable and unambiguous.
type Campaign struct {
	ID               string `json:"id"`
	AdvertiserID     string `json:"advertiser_id,omitempty"`
	Name             string `json:"name"`
	BudgetCents      int64  `json:"budget_cents"`
	DailyBudgetCents int64  `json:"daily_budget_cents"`
	Status           string `json:"status"`
	StartDate        string `json:"start_date"`
	EndDate          string `json:"end_date"`
}

// Ad is the API representation of an ad.
type Ad struct {
	ID             string   `json:"id"`
	CampaignID     string   `json:"campaign_id"`
	Title          string   `json:"title"`
	TargetKeywords []string `json:"target_keywords"`
	BidCents       int64    `json:"bid_cents"`
	Status         string   `json:"status"`
}

// ServedAd is the /serve response body.
type ServedAd struct {
	AdID       string `json:"ad_id"`
	CampaignID string `json:"campaign_id"`
	Title      string `json:"title"`
	BidCents   int64  `json:"bid_cents"`
}
