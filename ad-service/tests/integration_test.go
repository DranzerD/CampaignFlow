package tests

// Integration tests for the Ad Service. They need the real PostgreSQL, Redis
// and RabbitMQ from docker compose and are skipped unless INTEGRATION_TEST=1.
// See run-tests.sh at the repository root.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"ad-service/internal/cache"
	"ad-service/internal/db"
	"ad-service/internal/handlers"
	"ad-service/internal/models"
)

const testPassword = "password123"

type harness struct {
	api   *handlers.API
	store *db.Store
	cache *cache.Cache
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("INTEGRATION_TEST") != "1" {
		t.Skip("set INTEGRATION_TEST=1 to run integration tests")
	}
	ctx := context.Background()

	store, err := db.New(ctx, os.Getenv("AD_DATABASE_URL"))
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	if err := store.Migrate(ctx, migrationsDir()); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	redisCache, err := cache.New(ctx, os.Getenv("REDIS_URL"))
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		_ = redisCache.Close()
	})

	// Publisher is left nil: these tests cover serving and budget logic, and
	// the handlers treat publishing as fire-and-forget.
	return &harness{
		api: &handlers.API{
			Store:     store,
			Cache:     redisCache,
			JWTSecret: []byte("test-secret"),
		},
		store: store,
		cache: redisCache,
	}
}

func migrationsDir() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}
	return "../migrations"
}

// newAdvertiser seeds an advertiser and returns its id and a valid JWT.
func (h *harness) newAdvertiser(t *testing.T) (string, string) {
	t.Helper()
	id := uuid.NewString()
	email := "adv-" + id + "@example.com"
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := h.store.SeedAdvertiser(context.Background(), id, "Test Advertiser", email, string(hash)); err != nil {
		t.Fatalf("seed advertiser: %v", err)
	}

	rec := h.do(t, http.MethodPost, "/auth/login", "", map[string]string{
		"email": email, "password": testPassword,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token string `json:"token"`
	}
	decode(t, rec, &body)
	return id, body.Token
}

// do issues a request against the router and returns the recorded response.
func (h *harness) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		payload = raw
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.api.Router().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// createCampaign creates a campaign whose window includes today.
func (h *harness) createCampaign(t *testing.T, token string, budget, dailyBudget int64) models.Campaign {
	t.Helper()
	now := time.Now().UTC()
	rec := h.do(t, http.MethodPost, "/campaigns", token, map[string]any{
		"name":               "Test Campaign",
		"budget_cents":       budget,
		"daily_budget_cents": dailyBudget,
		"start_date":         now.AddDate(0, 0, -1).Format(models.DateLayout),
		"end_date":           now.AddDate(0, 0, 30).Format(models.DateLayout),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create campaign: %d %s", rec.Code, rec.Body.String())
	}
	var c models.Campaign
	decode(t, rec, &c)
	return c
}

func (h *harness) createAd(t *testing.T, token, campaignID, keyword string, bid int64, status string) models.Ad {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/campaigns/"+campaignID+"/ads", token, map[string]any{
		"title":           fmt.Sprintf("Ad %d", bid),
		"target_keywords": []string{keyword},
		"bid_cents":       bid,
		"status":          status,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create ad: %d %s", rec.Code, rec.Body.String())
	}
	var ad models.Ad
	decode(t, rec, &ad)
	return ad
}

// uniqueKeyword keeps each test isolated from the ads of the others.
func uniqueKeyword() string { return "kw-" + uuid.NewString() }

func (h *harness) serve(t *testing.T, keyword string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodGet, "/serve?keyword="+keyword+"&limit=1", "", nil)
}

func TestHealth(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodGet, "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health: %d", rec.Code)
	}
	var body map[string]string
	decode(t, rec, &body)
	if body["status"] != "ok" {
		t.Errorf("unexpected health body: %v", body)
	}
}

func TestCampaignLifecycle(t *testing.T) {
	h := newHarness(t)
	advertiserID, token := h.newAdvertiser(t)

	campaign := h.createCampaign(t, token, 100000, 10000)
	if campaign.Status != models.StatusActive {
		t.Errorf("new campaigns must default to active, got %q", campaign.Status)
	}

	// Fetching is public and exposes the owner.
	rec := h.do(t, http.MethodGet, "/campaigns/"+campaign.ID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get campaign: %d %s", rec.Code, rec.Body.String())
	}
	var fetched models.Campaign
	decode(t, rec, &fetched)
	if fetched.AdvertiserID != advertiserID {
		t.Errorf("expected advertiser %s, got %s", advertiserID, fetched.AdvertiserID)
	}

	// Patching applies only the supplied fields.
	rec = h.do(t, http.MethodPatch, "/campaigns/"+campaign.ID, token, map[string]any{
		"budget_cents": 120000,
		"status":       models.StatusPaused,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch campaign: %d %s", rec.Code, rec.Body.String())
	}
	var updated models.Campaign
	decode(t, rec, &updated)
	if updated.BudgetCents != 120000 || updated.Status != models.StatusPaused {
		t.Errorf("patch did not apply: %+v", updated)
	}
	if updated.DailyBudgetCents != 10000 {
		t.Errorf("omitted fields must be left alone, got %d", updated.DailyBudgetCents)
	}

	// Unknown campaigns are 404, not 500.
	rec = h.do(t, http.MethodGet, "/campaigns/"+uuid.NewString(), "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown campaign, got %d", rec.Code)
	}
}

func TestCreateAd(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	campaign := h.createCampaign(t, token, 100000, 10000)

	ad := h.createAd(t, token, campaign.ID, "gaming", 150, models.StatusActive)
	if ad.CampaignID != campaign.ID {
		t.Errorf("ad attached to wrong campaign: %+v", ad)
	}
	if len(ad.TargetKeywords) != 1 || ad.TargetKeywords[0] != "gaming" {
		t.Errorf("keywords not round-tripped: %+v", ad.TargetKeywords)
	}
}

func TestAuthenticationIsEnforced(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	campaign := h.createCampaign(t, token, 100000, 10000)

	cases := []struct {
		name, method, path, token string
		want                      int
	}{
		{"no token", http.MethodPost, "/campaigns", "", http.StatusUnauthorized},
		{"garbage token", http.MethodPatch, "/campaigns/" + campaign.ID, "not-a-jwt", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, tc.method, tc.path, tc.token, map[string]any{"status": "paused"})
			if rec.Code != tc.want {
				t.Errorf("expected %d, got %d (%s)", tc.want, rec.Code, rec.Body.String())
			}
		})
	}

	// Wrong credentials never mint a token.
	rec := h.do(t, http.MethodPost, "/auth/login", "", map[string]string{
		"email": "advertiser@example.com", "password": "wrong",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for bad password, got %d", rec.Code)
	}
}

func TestAdvertiserCannotModifyForeignCampaign(t *testing.T) {
	h := newHarness(t)
	_, ownerToken := h.newAdvertiser(t)
	_, otherToken := h.newAdvertiser(t)
	campaign := h.createCampaign(t, ownerToken, 100000, 10000)

	rec := h.do(t, http.MethodPatch, "/campaigns/"+campaign.ID, otherToken,
		map[string]any{"status": models.StatusPaused})
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 on foreign campaign patch, got %d %s", rec.Code, rec.Body.String())
	}

	rec = h.do(t, http.MethodPost, "/campaigns/"+campaign.ID+"/ads", otherToken, map[string]any{
		"title": "Sneaky Ad", "target_keywords": []string{"gaming"}, "bid_cents": 100,
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 on foreign ad creation, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestServePicksHighestBid(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	campaign := h.createCampaign(t, token, 1000000, 100000)
	keyword := uniqueKeyword()

	h.createAd(t, token, campaign.ID, keyword, 100, models.StatusActive)
	winner := h.createAd(t, token, campaign.ID, keyword, 300, models.StatusActive)
	h.createAd(t, token, campaign.ID, keyword, 200, models.StatusActive)

	rec := h.serve(t, keyword)
	if rec.Code != http.StatusOK {
		t.Fatalf("serve: %d %s", rec.Code, rec.Body.String())
	}
	var served models.ServedAd
	decode(t, rec, &served)
	if served.AdID != winner.ID {
		t.Errorf("expected the 300c ad to win, got %+v", served)
	}
}

func TestServeTieBreaksOnOlderAd(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	campaign := h.createCampaign(t, token, 1000000, 100000)
	keyword := uniqueKeyword()

	older := h.createAd(t, token, campaign.ID, keyword, 150, models.StatusActive)
	h.createAd(t, token, campaign.ID, keyword, 150, models.StatusActive)

	rec := h.serve(t, keyword)
	var served models.ServedAd
	decode(t, rec, &served)
	if served.AdID != older.ID {
		t.Errorf("expected the older ad to win the tie, got %+v", served)
	}
}

func TestServeExcludesPausedAdsAndCampaigns(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)

	t.Run("paused ad", func(t *testing.T) {
		campaign := h.createCampaign(t, token, 1000000, 100000)
		keyword := uniqueKeyword()
		h.createAd(t, token, campaign.ID, keyword, 500, models.StatusPaused)
		if rec := h.serve(t, keyword); rec.Code != http.StatusNotFound {
			t.Errorf("paused ad should not serve, got %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("paused campaign", func(t *testing.T) {
		campaign := h.createCampaign(t, token, 1000000, 100000)
		keyword := uniqueKeyword()
		h.createAd(t, token, campaign.ID, keyword, 500, models.StatusActive)
		rec := h.do(t, http.MethodPatch, "/campaigns/"+campaign.ID, token,
			map[string]any{"status": models.StatusPaused})
		if rec.Code != http.StatusOK {
			t.Fatalf("pause campaign: %d %s", rec.Code, rec.Body.String())
		}
		if rec := h.serve(t, keyword); rec.Code != http.StatusNotFound {
			t.Errorf("paused campaign should not serve, got %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestServeExcludesExpiredCampaign(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	keyword := uniqueKeyword()

	now := time.Now().UTC()
	rec := h.do(t, http.MethodPost, "/campaigns", token, map[string]any{
		"name":               "Expired Campaign",
		"budget_cents":       100000,
		"daily_budget_cents": 10000,
		"start_date":         now.AddDate(0, 0, -10).Format(models.DateLayout),
		"end_date":           now.AddDate(0, 0, -1).Format(models.DateLayout),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create campaign: %d %s", rec.Code, rec.Body.String())
	}
	var campaign models.Campaign
	decode(t, rec, &campaign)
	h.createAd(t, token, campaign.ID, keyword, 500, models.StatusActive)

	if rec := h.serve(t, keyword); rec.Code != http.StatusNotFound {
		t.Errorf("expired campaign should not serve, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestDailyBudgetStopsServing(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	keyword := uniqueKeyword()

	// Daily budget of 300c against a 100c bid: exactly three impressions.
	campaign := h.createCampaign(t, token, 1000000, 300)
	h.createAd(t, token, campaign.ID, keyword, 100, models.StatusActive)

	for i := 0; i < 3; i++ {
		if rec := h.serve(t, keyword); rec.Code != http.StatusOK {
			t.Fatalf("serve %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := h.serve(t, keyword); rec.Code != http.StatusNotFound {
		t.Errorf("fourth serve should exceed the daily budget, got %d %s", rec.Code, rec.Body.String())
	}

	spend, err := h.cache.DailySpend(context.Background(), campaign.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("read daily spend: %v", err)
	}
	if spend != 300 {
		t.Errorf("expected 300c booked, got %d", spend)
	}
}

func TestTotalBudgetStopsServing(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	keyword := uniqueKeyword()

	// Here the total budget binds before the daily one does.
	campaign := h.createCampaign(t, token, 200, 100000)
	h.createAd(t, token, campaign.ID, keyword, 100, models.StatusActive)

	for i := 0; i < 2; i++ {
		if rec := h.serve(t, keyword); rec.Code != http.StatusOK {
			t.Fatalf("serve %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := h.serve(t, keyword); rec.Code != http.StatusNotFound {
		t.Errorf("third serve should exceed the total budget, got %d", rec.Code)
	}
}

func TestConcurrentServeDoesNotOverspend(t *testing.T) {
	h := newHarness(t)
	_, token := h.newAdvertiser(t)
	keyword := uniqueKeyword()

	const (
		bid         = 100
		dailyBudget = 2000 // room for exactly 20 impressions
		requests    = 200
	)
	campaign := h.createCampaign(t, token, 1000000, dailyBudget)
	h.createAd(t, token, campaign.ID, keyword, bid, models.StatusActive)

	// Warm the keyword cache so the goroutines race on the spend counter
	// rather than on the first database read.
	if rec := h.serve(t, keyword); rec.Code != http.StatusOK {
		t.Fatalf("warmup serve: %d %s", rec.Code, rec.Body.String())
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		served = 1 // the warmup impression
	)
	router := h.api.Router()
	start := make(chan struct{})
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/serve?keyword="+keyword+"&limit=1", nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				mu.Lock()
				served++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if want := dailyBudget / bid; served != want {
		t.Errorf("expected exactly %d impressions, got %d", want, served)
	}

	spend, err := h.cache.DailySpend(context.Background(), campaign.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("read daily spend: %v", err)
	}
	if spend > dailyBudget {
		t.Errorf("daily budget overspent: %d > %d", spend, dailyBudget)
	}
}

func TestServeRequiresKeyword(t *testing.T) {
	h := newHarness(t)
	if rec := h.do(t, http.MethodGet, "/serve", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without a keyword, got %d", rec.Code)
	}
}

func TestClickRequiresKnownAd(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, http.MethodPost, "/events/click?ad_id="+uuid.NewString(), "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown ad, got %d %s", rec.Code, rec.Body.String())
	}
}
