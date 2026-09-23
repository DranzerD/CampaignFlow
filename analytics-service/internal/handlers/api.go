package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"analytics-service/internal/db"
	"analytics-service/internal/models"
)

// API holds everything the Analytics HTTP handlers need.
type API struct {
	Store *db.Store
}

// Router wires the Analytics routes.
func (a *API) Router() http.Handler {
	r := chi.NewRouter()

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/reports/campaign/{id}", a.CampaignReport)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "resource does not exist")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "method not allowed for this resource")
	})
	return r
}

// CampaignReport reports one campaign-day, suppressing the numbers when the
// privacy threshold is not met.
func (a *API) CampaignReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "campaign id must be a uuid")
		return
	}

	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().UTC().Format(models.DateLayout)
	}
	if _, err := time.Parse(models.DateLayout, date); err != nil {
		writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}

	impressions, clicks, err := a.Store.Stats(r.Context(), id, date)
	if err != nil {
		log.Printf("stats lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "unexpected server error")
		return
	}
	writeJSON(w, http.StatusOK, models.BuildReport(id, date, impressions, clicks))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("failed to write response: %v", err)
	}
}

// writeError emits the single error shape used by the whole API.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
