package handlers

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"ad-service/internal/models"
)

type createAdRequest struct {
	Title          string   `json:"title"`
	TargetKeywords []string `json:"target_keywords"`
	BidCents       int64    `json:"bid_cents"`
	Status         string   `json:"status"`
}

// CreateAd adds an ad to a campaign. Only the campaign owner may do this.
func (a *API) CreateAd(w http.ResponseWriter, r *http.Request) {
	campaign, ok := a.ownedCampaign(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}

	var req createAdRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len(req.TargetKeywords) == 0 {
		writeError(w, http.StatusBadRequest, "at least one target keyword is required")
		return
	}
	if req.BidCents <= 0 {
		writeError(w, http.StatusBadRequest, "bid_cents must be greater than zero")
		return
	}
	if req.Status == "" {
		req.Status = models.StatusActive
	}
	if !models.ValidStatus(req.Status) {
		writeError(w, http.StatusBadRequest, "status must be active or paused")
		return
	}

	ad, err := a.Store.CreateAd(r.Context(), uuid.NewString(), campaign.ID,
		req.Title, req.TargetKeywords, req.BidCents, req.Status)
	if err != nil {
		log.Printf("create ad failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}
	a.invalidateCache(r)
	writeJSON(w, http.StatusCreated, ad)
}
