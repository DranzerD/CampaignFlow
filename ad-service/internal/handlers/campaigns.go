package handlers

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ad-service/internal/db"
	"ad-service/internal/models"
)

type createCampaignRequest struct {
	Name             string `json:"name"`
	BudgetCents      int64  `json:"budget_cents"`
	DailyBudgetCents int64  `json:"daily_budget_cents"`
	StartDate        string `json:"start_date"`
	EndDate          string `json:"end_date"`
}

// CreateCampaign creates a campaign owned by the authenticated advertiser.
func (a *API) CreateCampaign(w http.ResponseWriter, r *http.Request) {
	var req createCampaignRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.BudgetCents < 0 || req.DailyBudgetCents < 0 {
		writeError(w, http.StatusBadRequest, "budgets must be zero or greater")
		return
	}
	start, err := time.Parse(models.DateLayout, req.StartDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "start_date must be YYYY-MM-DD")
		return
	}
	end, err := time.Parse(models.DateLayout, req.EndDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "end_date must be YYYY-MM-DD")
		return
	}
	if end.Before(start) {
		writeError(w, http.StatusBadRequest, "end_date must not be before start_date")
		return
	}

	campaign, err := a.Store.CreateCampaign(r.Context(), uuid.NewString(),
		advertiserFromContext(r.Context()), req.Name,
		req.BudgetCents, req.DailyBudgetCents, req.StartDate, req.EndDate)
	if err != nil {
		log.Printf("create campaign failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}
	a.invalidateCache(r)

	campaign.AdvertiserID = ""
	writeJSON(w, http.StatusCreated, campaign)
}

// GetCampaign returns a campaign. This endpoint is public.
func (a *API) GetCampaign(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "campaign id must be a uuid")
		return
	}
	campaign, err := a.Store.GetCampaign(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "campaign does not exist")
		return
	}
	if err != nil {
		log.Printf("get campaign failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}
	writeJSON(w, http.StatusOK, campaign)
}

type updateCampaignRequest struct {
	BudgetCents      *int64  `json:"budget_cents"`
	DailyBudgetCents *int64  `json:"daily_budget_cents"`
	Status           *string `json:"status"`
}

// UpdateCampaign patches budget or status. Only the owner may do this.
func (a *API) UpdateCampaign(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	campaign, ok := a.ownedCampaign(w, r, id)
	if !ok {
		return
	}

	var req updateCampaignRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.BudgetCents != nil && *req.BudgetCents < 0 {
		writeError(w, http.StatusBadRequest, "budget_cents must be zero or greater")
		return
	}
	if req.DailyBudgetCents != nil && *req.DailyBudgetCents < 0 {
		writeError(w, http.StatusBadRequest, "daily_budget_cents must be zero or greater")
		return
	}
	if req.Status != nil && !models.ValidStatus(*req.Status) {
		writeError(w, http.StatusBadRequest, "status must be active or paused")
		return
	}

	updated, err := a.Store.UpdateCampaign(r.Context(), campaign.ID,
		req.BudgetCents, req.DailyBudgetCents, req.Status)
	if err != nil {
		log.Printf("update campaign failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}
	a.invalidateCache(r)
	writeJSON(w, http.StatusOK, updated)
}

// ownedCampaign loads a campaign and checks that the caller owns it.
func (a *API) ownedCampaign(w http.ResponseWriter, r *http.Request, id string) (*models.Campaign, bool) {
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "campaign id must be a uuid")
		return nil, false
	}
	campaign, err := a.Store.GetCampaign(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "campaign does not exist")
		return nil, false
	}
	if err != nil {
		log.Printf("load campaign failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return nil, false
	}
	if campaign.AdvertiserID != advertiserFromContext(r.Context()) {
		writeError(w, http.StatusForbidden, "campaign belongs to another advertiser")
		return nil, false
	}
	return campaign, true
}

// invalidateCache clears the keyword cache after any write that can change ad
// eligibility.
func (a *API) invalidateCache(r *http.Request) {
	if a.Cache == nil {
		return
	}
	if err := a.Cache.InvalidateKeywords(r.Context()); err != nil {
		log.Printf("cache invalidation failed: %v", err)
	}
}
