package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"analytics-service/internal/models"
)

// Store owns the Analytics database connection pool.
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

// Migrate runs every .sql file in dir in lexical order.
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

// RecordEvent stores a raw event and folds it into the daily campaign
// aggregate in one transaction.
//
// The event UUID is the idempotency key: a redelivered message hits the
// primary key conflict on events_raw, no rows are inserted, and the aggregate
// is left alone. Both statements share a transaction, so the raw event and the
// counter can never disagree.
func (s *Store) RecordEvent(ctx context.Context, e models.Event) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`INSERT INTO events_raw (id, ad_id, campaign_id, event_type, timestamp)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (id) DO NOTHING`,
		e.ID, e.AdID, e.CampaignID, e.EventType, e.Timestamp.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Duplicate delivery: already counted.
		return tx.Commit(ctx)
	}

	var impressions, clicks int64
	if e.EventType == models.EventImpression {
		impressions = 1
	} else {
		clicks = 1
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO campaign_stats (campaign_id, date, impressions, clicks)
		 VALUES ($1, $2::date, $3, $4)
		 ON CONFLICT (campaign_id, date) DO UPDATE SET
			impressions = campaign_stats.impressions + EXCLUDED.impressions,
			clicks      = campaign_stats.clicks + EXCLUDED.clicks`,
		e.CampaignID, e.Timestamp.UTC().Format(models.DateLayout), impressions, clicks); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Stats returns the impressions and clicks recorded for a campaign on a date.
// A campaign-day with no events reports zeroes rather than an error.
func (s *Store) Stats(ctx context.Context, campaignID, date string) (impressions, clicks int64, err error) {
	err = s.Pool.QueryRow(ctx,
		`SELECT impressions, clicks FROM campaign_stats
		 WHERE campaign_id = $1 AND date = $2::date`, campaignID, date).
		Scan(&impressions, &clicks)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return impressions, clicks, err
}
