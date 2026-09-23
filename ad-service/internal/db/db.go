package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ad-service/internal/models"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Store owns the Ad Service database connection pool.
type Store struct {
	Pool *pgxpool.Pool
}

// New opens the pool and waits until the database answers.
func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	for i := 0; i < 30; i++ {
		if err = pool.Ping(ctx); err == nil {
			log.Println("connected to postgres")
			return &Store{Pool: pool}, nil
		}
		log.Printf("waiting for postgres: %v", err)
		time.Sleep(2 * time.Second)
	}
	pool.Close()
	return nil, fmt.Errorf("postgres not reachable: %w", err)
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate runs every .sql file in dir in lexical order. The files are written
// to be idempotent, so re-running them on an existing database is a no-op.
func (s *Store) Migrate(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		log.Printf("applied migration %s", name)
	}
	return nil
}

// SeedAdvertiser inserts the demo advertiser used for logging in. It is a
// no-op once the row exists.
func (s *Store) SeedAdvertiser(ctx context.Context, id, name, email, passwordHash string) error {
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO advertisers (id, name, email, password_hash)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (email) DO NOTHING`,
		id, name, email, passwordHash)
	return err
}

// Advertiser is the minimal advertiser record needed for login.
type Advertiser struct {
	ID           string
	Email        string
	PasswordHash string
}

func (s *Store) AdvertiserByEmail(ctx context.Context, email string) (*Advertiser, error) {
	var a Advertiser
	err := s.Pool.QueryRow(ctx,
		`SELECT id::text, email, password_hash FROM advertisers WHERE email = $1`, email).
		Scan(&a.ID, &a.Email, &a.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

const campaignCols = `id::text, advertiser_id::text, name, budget_cents,
	daily_budget_cents, status, to_char(start_date, 'YYYY-MM-DD'),
	to_char(end_date, 'YYYY-MM-DD')`

func scanCampaign(row pgx.Row) (*models.Campaign, error) {
	var c models.Campaign
	err := row.Scan(&c.ID, &c.AdvertiserID, &c.Name, &c.BudgetCents,
		&c.DailyBudgetCents, &c.Status, &c.StartDate, &c.EndDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) CreateCampaign(ctx context.Context, id, advertiserID, name string,
	budgetCents, dailyBudgetCents int64, startDate, endDate string) (*models.Campaign, error) {

	row := s.Pool.QueryRow(ctx,
		`INSERT INTO campaigns (id, advertiser_id, name, budget_cents,
			daily_budget_cents, status, start_date, end_date)
		 VALUES ($1, $2, $3, $4, $5, 'active', $6::date, $7::date)
		 RETURNING `+campaignCols,
		id, advertiserID, name, budgetCents, dailyBudgetCents, startDate, endDate)
	return scanCampaign(row)
}

func (s *Store) GetCampaign(ctx context.Context, id string) (*models.Campaign, error) {
	return scanCampaign(s.Pool.QueryRow(ctx,
		`SELECT `+campaignCols+` FROM campaigns WHERE id = $1`, id))
}

// UpdateCampaign applies the non-nil fields and returns the updated row.
// COALESCE keeps the statement a single parameterized UPDATE.
func (s *Store) UpdateCampaign(ctx context.Context, id string,
	budgetCents, dailyBudgetCents *int64, status *string) (*models.Campaign, error) {

	return scanCampaign(s.Pool.QueryRow(ctx,
		`UPDATE campaigns SET
			budget_cents       = COALESCE($2::bigint, budget_cents),
			daily_budget_cents = COALESCE($3::bigint, daily_budget_cents),
			status             = COALESCE($4::text, status)
		 WHERE id = $1
		 RETURNING `+campaignCols,
		id, budgetCents, dailyBudgetCents, status))
}

func (s *Store) CreateAd(ctx context.Context, id, campaignID, title string,
	keywords []string, bidCents int64, status string) (*models.Ad, error) {

	kw, err := json.Marshal(keywords)
	if err != nil {
		return nil, err
	}
	var ad models.Ad
	var raw []byte
	err = s.Pool.QueryRow(ctx,
		`INSERT INTO ads (id, campaign_id, title, target_keywords, bid_cents, status)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6)
		 RETURNING id::text, campaign_id::text, title, target_keywords, bid_cents, status`,
		id, campaignID, title, string(kw), bidCents, status).
		Scan(&ad.ID, &ad.CampaignID, &ad.Title, &raw, &ad.BidCents, &ad.Status)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &ad.TargetKeywords); err != nil {
		return nil, err
	}
	return &ad, nil
}

// CampaignIDForAd resolves the campaign an ad belongs to (needed to publish
// click events, which only carry an ad id).
func (s *Store) CampaignIDForAd(ctx context.Context, adID string) (string, error) {
	var campaignID string
	err := s.Pool.QueryRow(ctx,
		`SELECT campaign_id::text FROM ads WHERE id = $1`, adID).Scan(&campaignID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return campaignID, err
}

// CandidatesByKeyword returns every ad targeting keyword together with its
// campaign. Status/date filtering and ranking happen in models.RankCandidates
// so that the cached set stays valid for the whole day.
func (s *Store) CandidatesByKeyword(ctx context.Context, keyword string) ([]models.Candidate, error) {
	kw, err := json.Marshal([]string{keyword})
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT a.id::text, a.campaign_id::text, a.title, a.bid_cents, a.status,
		        c.status, to_char(c.start_date, 'YYYY-MM-DD'),
		        to_char(c.end_date, 'YYYY-MM-DD'),
		        c.budget_cents, c.daily_budget_cents, a.created_at
		 FROM ads a
		 JOIN campaigns c ON c.id = a.campaign_id
		 WHERE a.target_keywords @> $1::jsonb`, string(kw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Candidate
	for rows.Next() {
		var c models.Candidate
		if err := rows.Scan(&c.AdID, &c.CampaignID, &c.Title, &c.BidCents,
			&c.AdStatus, &c.CampaignStatus, &c.StartDate, &c.EndDate,
			&c.BudgetCents, &c.DailyBudgetCents, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
