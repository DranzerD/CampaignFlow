package models

import (
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

// DateLayout is the layout used for every date crossing the API boundary.
const DateLayout = "2006-01-02"

// Event types published by the Ad Service.
const (
	EventImpression = "impression"
	EventClick      = "click"
)

// PrivacyThreshold is the minimum number of impressions a campaign-day must
// have before its statistics are reported. Below it, the report says only
// "insufficient data" -- see the privacy section of the README.
const PrivacyThreshold = 50

// ErrInvalidEvent marks a message that can never succeed and must not be
// retried forever.
var ErrInvalidEvent = errors.New("invalid event")

// Event is the message contract shared with the Ad Service.
type Event struct {
	ID         string    `json:"id"`
	AdID       string    `json:"ad_id"`
	CampaignID string    `json:"campaign_id"`
	EventType  string    `json:"event_type"`
	Timestamp  time.Time `json:"timestamp"`
}

// Validate rejects malformed events before they reach the database. ad_id,
// campaign_id and id must be well-formed UUIDs -- not just non-empty -- since
// events_raw and campaign_stats both store them as the UUID column type. A
// value that merely passed an empty-string check would fail the INSERT and,
// left unguarded, get nacked-and-requeued by the consumer forever because the
// same malformed value would fail on every redelivery too.
func (e Event) Validate() error {
	switch {
	case e.ID == "":
		return errors.Join(ErrInvalidEvent, errors.New("missing id"))
	case !isUUID(e.ID):
		return errors.Join(ErrInvalidEvent, errors.New("id is not a valid uuid"))
	case e.AdID == "":
		return errors.Join(ErrInvalidEvent, errors.New("missing ad_id"))
	case !isUUID(e.AdID):
		return errors.Join(ErrInvalidEvent, errors.New("ad_id is not a valid uuid"))
	case e.CampaignID == "":
		return errors.Join(ErrInvalidEvent, errors.New("missing campaign_id"))
	case !isUUID(e.CampaignID):
		return errors.Join(ErrInvalidEvent, errors.New("campaign_id is not a valid uuid"))
	case e.EventType != EventImpression && e.EventType != EventClick:
		return errors.Join(ErrInvalidEvent, errors.New("unknown event_type"))
	case e.Timestamp.IsZero():
		return errors.Join(ErrInvalidEvent, errors.New("missing timestamp"))
	}
	return nil
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// Report is the /reports/campaign/{id} response. Impressions, clicks and CTR
// are omitted entirely when the privacy threshold is not met.
type Report struct {
	CampaignID  string   `json:"campaign_id"`
	Date        string   `json:"date"`
	Impressions *int64   `json:"impressions,omitempty"`
	Clicks      *int64   `json:"clicks,omitempty"`
	CTR         *float64 `json:"ctr,omitempty"`
	Status      string   `json:"status,omitempty"`
}

// BuildReport applies the CTR calculation and the privacy suppression rule.
func BuildReport(campaignID, date string, impressions, clicks int64) Report {
	if impressions < PrivacyThreshold {
		return Report{CampaignID: campaignID, Date: date, Status: "insufficient data"}
	}
	ctr := CTR(impressions, clicks)
	return Report{
		CampaignID:  campaignID,
		Date:        date,
		Impressions: &impressions,
		Clicks:      &clicks,
		CTR:         &ctr,
	}
}

// CTR is clicks / impressions, rounded to four decimals and guarded against
// division by zero.
func CTR(impressions, clicks int64) float64 {
	if impressions <= 0 {
		return 0
	}
	return math.Round(float64(clicks)/float64(impressions)*10000) / 10000
}
