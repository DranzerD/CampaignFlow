// Command analytics-service runs the RabbitMQ consumer and the reporting API
// in a single process, on separate goroutines.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"analytics-service/internal/db"
	"analytics-service/internal/handlers"
	"analytics-service/internal/queue"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Println("analytics-service starting")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.New(ctx, env("ANALYTICS_DATABASE_URL",
		"postgres://analytics:analytics@localhost:5433/analytics"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(ctx, env("MIGRATIONS_DIR", "migrations")); err != nil {
		log.Fatalf("migrations: %v", err)
	}

	consumer := queue.NewConsumer(
		env("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"),
		env("RABBITMQ_EXCHANGE", "ad-events"),
		env("RABBITMQ_QUEUE", "analytics-events"),
		store)
	go consumer.Run(ctx)

	api := &handlers.API{Store: store}
	addr := ":" + env("ANALYTICS_SERVICE_PORT", "8081")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("analytics-service listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("http server: %v", err)
	}
}
