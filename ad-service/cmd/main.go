// Command ad-service runs the Ad Service HTTP API.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ad-service/internal/cache"
	"ad-service/internal/db"
	"ad-service/internal/handlers"
	"ad-service/internal/queue"
)

// Demo advertiser created on first start so that /auth/login works out of the
// box. Documented in the README; not a production practice.
const (
	seedAdvertiserID       = "11111111-1111-1111-1111-111111111111"
	seedAdvertiserName     = "Demo Advertiser"
	seedAdvertiserEmail    = "advertiser@example.com"
	seedAdvertiserPassword = "password123"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Println("ad-service starting")

	ctx := context.Background()

	store, err := db.New(ctx, env("AD_DATABASE_URL", "postgres://ads:ads@localhost:5432/ads"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(ctx, env("MIGRATIONS_DIR", "migrations")); err != nil {
		log.Fatalf("migrations: %v", err)
	}
	if err := seedAdvertiser(ctx, store); err != nil {
		log.Fatalf("seed advertiser: %v", err)
	}

	redisCache, err := cache.New(ctx, env("REDIS_URL", "redis://localhost:6379"))
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	defer redisCache.Close()

	publisher, err := queue.NewPublisher(
		env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"),
		env("RABBITMQ_EXCHANGE", "ad-events"))
	if err != nil {
		log.Fatalf("rabbitmq: %v", err)
	}
	defer publisher.Close()

	api := &handlers.API{
		Store:     store,
		Cache:     redisCache,
		Publisher: publisher,
		JWTSecret: []byte(env("JWT_SECRET", "change-me-in-production")),
	}

	addr := ":" + env("AD_SERVICE_PORT", "8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("ad-service listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

func seedAdvertiser(ctx context.Context, store *db.Store) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(seedAdvertiserPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := store.SeedAdvertiser(ctx, seedAdvertiserID, seedAdvertiserName,
		seedAdvertiserEmail, string(hash)); err != nil {
		return err
	}
	log.Printf("demo advertiser available: %s", seedAdvertiserEmail)
	return nil
}
