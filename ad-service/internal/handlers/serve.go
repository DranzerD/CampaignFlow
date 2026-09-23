package handlers

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"ad-service/internal/db"
	"ad-service/internal/models"
	"ad-service/internal/queue"
)

// Serve picks the best eligible ad for a keyword, books its bid against the
// campaign budgets and publishes an impression event.
func (a *API) Serve(w http.ResponseWriter, r *http.Request) {
	keyword := r.URL.Query().Get("keyword")
	if keyword == "" {
		writeError(w, http.StatusBadRequest, "keyword is required")
		return
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
	}

	candidates, err := a.candidates(r, keyword)
	if err != nil {
		log.Printf("candidate lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}

	now := a.now()
	// Candidates come back best-first; the first one with budget left wins.
	for _, c := range models.RankCandidates(candidates, now) {
		reserved, err := a.Cache.ReserveBudget(r.Context(), c.CampaignID,
			c.BidCents, c.DailyBudgetCents, c.BudgetCents, now)
		if err != nil {
			log.Printf("budget reservation failed for campaign %s: %v", c.CampaignID, err)
			writeError(w, http.StatusInternalServerError, "unexpected server error")
			return
		}
		if !reserved {
			continue
		}

		if a.Publisher != nil {
			a.Publisher.Publish(queue.Event{
				ID:         uuid.NewString(),
				AdID:       c.AdID,
				CampaignID: c.CampaignID,
				EventType:  queue.EventImpression,
				Timestamp:  now.UTC().Truncate(time.Second),
			})
		}
		writeJSON(w, http.StatusOK, models.ServedAd{
			AdID:       c.AdID,
			CampaignID: c.CampaignID,
			Title:      c.Title,
			BidCents:   c.BidCents,
		})
		return
	}

	writeError(w, http.StatusNotFound, "no eligible ad found")
}

// candidates reads the keyword's ads from Redis, falling back to PostgreSQL.
func (a *API) candidates(r *http.Request, keyword string) ([]models.Candidate, error) {
	if a.Cache != nil {
		if cached, ok := a.Cache.GetCandidates(r.Context(), keyword); ok {
			return cached, nil
		}
	}
	cands, err := a.Store.CandidatesByKeyword(r.Context(), keyword)
	if err != nil {
		return nil, err
	}
	if a.Cache != nil {
		a.Cache.SetCandidates(r.Context(), keyword, cands)
	}
	return cands, nil
}

// RecordClick publishes a click event for an ad. It returns as soon as the
// event is queued; Analytics processes it asynchronously.
func (a *API) RecordClick(w http.ResponseWriter, r *http.Request) {
	adID := r.URL.Query().Get("ad_id")
	if _, err := uuid.Parse(adID); err != nil {
		writeError(w, http.StatusBadRequest, "ad_id must be a uuid")
		return
	}

	campaignID, err := a.Store.CampaignIDForAd(r.Context(), adID)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "ad does not exist")
		return
	}
	if err != nil {
		log.Printf("click lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}

	if a.Publisher != nil {
		a.Publisher.Publish(queue.Event{
			ID:         uuid.NewString(),
			AdID:       adID,
			CampaignID: campaignID,
			EventType:  queue.EventClick,
			Timestamp:  a.now().UTC().Truncate(time.Second),
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
