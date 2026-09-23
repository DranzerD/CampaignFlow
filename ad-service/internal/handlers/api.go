package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"ad-service/internal/cache"
	"ad-service/internal/db"
	"ad-service/internal/queue"
)

// API holds everything the HTTP handlers need.
type API struct {
	Store     *db.Store
	Cache     *cache.Cache
	Publisher *queue.Publisher
	JWTSecret []byte
	// Now is injectable so tests can serve ads "on" a specific date.
	Now func() time.Time
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

// Router wires every route of the Ad Service.
func (a *API) Router() http.Handler {
	r := chi.NewRouter()

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Post("/auth/login", a.Login)

	r.Get("/campaigns/{id}", a.GetCampaign)
	r.Group(func(pr chi.Router) {
		pr.Use(a.RequireAuth)
		pr.Post("/campaigns", a.CreateCampaign)
		pr.Patch("/campaigns/{id}", a.UpdateCampaign)
		pr.Post("/campaigns/{id}/ads", a.CreateAd)
	})

	r.Get("/serve", a.Serve)
	r.Post("/events/click", a.RecordClick)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "resource does not exist")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "method not allowed for this resource")
	})
	return r
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

// decodeJSON reads a JSON body, rejecting unknown fields so typos in requests
// surface as 400s instead of being silently ignored.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
