package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"ad-service/internal/models"
)

// candidateTTL keeps the keyword cache short-lived on top of the explicit
// invalidation done on every write, so a missed invalidation self-heals.
const candidateTTL = 60 * time.Second

// reserveScript performs "check daily spend + check total spend + increment"
// in one atomic step. Doing these as separate GET/INCR round trips would let
// concurrent /serve requests overspend the budget.
//
// KEYS[1] daily spend key, KEYS[2] total spend key
// ARGV[1] bid, ARGV[2] daily budget, ARGV[3] total budget, ARGV[4] daily TTL
// returns 1 on success, -1 daily budget exhausted, -2 total budget exhausted
var reserveScript = redis.NewScript(`
local daily = tonumber(redis.call('GET', KEYS[1]) or '0')
local total = tonumber(redis.call('GET', KEYS[2]) or '0')
local bid = tonumber(ARGV[1])
if daily + bid > tonumber(ARGV[2]) then return -1 end
if total + bid > tonumber(ARGV[3]) then return -2 end
redis.call('INCRBY', KEYS[1], bid)
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[4]))
redis.call('INCRBY', KEYS[2], bid)
return 1
`)

// Cache wraps the two legitimate uses of Redis here: caching the ads that
// target a keyword, and atomic daily/total spend accounting.
type Cache struct {
	rdb *redis.Client
}

// New parses a redis:// URL and verifies the connection.
func New(ctx context.Context, url string) (*Cache, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opts)
	var pingErr error
	for i := 0; i < 30; i++ {
		if pingErr = rdb.Ping(ctx).Err(); pingErr == nil {
			log.Println("connected to redis")
			return &Cache{rdb: rdb}, nil
		}
		log.Printf("waiting for redis: %v", pingErr)
		time.Sleep(2 * time.Second)
	}
	_ = rdb.Close()
	return nil, fmt.Errorf("redis not reachable: %w", pingErr)
}

func (c *Cache) Close() error { return c.rdb.Close() }

func keywordKey(keyword string) string { return "ads:keyword:" + keyword }

func DailySpendKey(campaignID string, now time.Time) string {
	return fmt.Sprintf("campaign:%s:daily_spend:%s", campaignID, now.UTC().Format("2006-01-02"))
}

func totalSpendKey(campaignID string) string {
	return fmt.Sprintf("campaign:%s:total_spend", campaignID)
}

// GetCandidates returns the cached candidate list for a keyword.
func (c *Cache) GetCandidates(ctx context.Context, keyword string) ([]models.Candidate, bool) {
	raw, err := c.rdb.Get(ctx, keywordKey(keyword)).Bytes()
	if err != nil {
		return nil, false
	}
	var cands []models.Candidate
	if err := json.Unmarshal(raw, &cands); err != nil {
		return nil, false
	}
	return cands, true
}

// SetCandidates caches the candidate list for a keyword. A cache write failure
// is not fatal for serving, so the error is only logged.
func (c *Cache) SetCandidates(ctx context.Context, keyword string, cands []models.Candidate) {
	raw, err := json.Marshal(cands)
	if err != nil {
		log.Printf("cache marshal failed for %q: %v", keyword, err)
		return
	}
	if err := c.rdb.Set(ctx, keywordKey(keyword), raw, candidateTTL).Err(); err != nil {
		log.Printf("cache write failed for %q: %v", keyword, err)
	}
}

// InvalidateKeywords drops the whole keyword cache. It is called whenever an
// ad or campaign changes; since the cached rows carry campaign status, budget
// and targeting, any of those edits can affect any keyword.
func (c *Cache) InvalidateKeywords(ctx context.Context) error {
	var cursor uint64
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, "ads:keyword:*", 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// ReserveBudget atomically books bidCents against the campaign's daily and
// total budgets. It returns false when the ad must not be served.
func (c *Cache) ReserveBudget(ctx context.Context, campaignID string,
	bidCents, dailyBudgetCents, totalBudgetCents int64, now time.Time) (bool, error) {

	res, err := reserveScript.Run(ctx, c.rdb,
		[]string{DailySpendKey(campaignID, now), totalSpendKey(campaignID)},
		bidCents, dailyBudgetCents, totalBudgetCents, secondsUntilEndOfDay(now),
	).Int64()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// DailySpend reports the amount already booked today (used by tests).
func (c *Cache) DailySpend(ctx context.Context, campaignID string, now time.Time) (int64, error) {
	v, err := c.rdb.Get(ctx, DailySpendKey(campaignID, now)).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

// secondsUntilEndOfDay makes the daily spend key expire at the end of the
// current UTC day.
func secondsUntilEndOfDay(now time.Time) int64 {
	utc := now.UTC()
	endOfDay := time.Date(utc.Year(), utc.Month(), utc.Day(), 23, 59, 59, 0, time.UTC)
	secs := int64(endOfDay.Sub(utc).Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	return secs
}
