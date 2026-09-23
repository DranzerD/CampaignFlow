package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"analytics-service/internal/db"
	"analytics-service/internal/models"
)

// Consumer reads ad events from RabbitMQ and writes them to PostgreSQL.
type Consumer struct {
	url      string
	exchange string
	queue    string
	store    *db.Store
}

func NewConsumer(url, exchange, queue string, store *db.Store) *Consumer {
	return &Consumer{url: url, exchange: exchange, queue: queue, store: store}
}

// Run consumes until ctx is cancelled, reconnecting when the broker drops the
// connection.
func (c *Consumer) Run(ctx context.Context) {
	for {
		if err := c.consume(ctx); err != nil {
			log.Printf("consumer stopped: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *Consumer) consume(ctx context.Context) error {
	conn, ch, err := c.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	defer ch.Close()

	deliveries, err := ch.Consume(c.queue, "", false /* manual ack */, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	log.Printf("consuming events from queue %q", c.queue)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("delivery channel closed")
			}
			c.handle(ctx, d)
		}
	}
}

// handle implements the reliability contract: validate, write to PostgreSQL,
// and only then ACK. A database failure leaves the message unacknowledged so
// RabbitMQ redelivers it.
func (c *Consumer) handle(ctx context.Context, d amqp.Delivery) {
	var e models.Event
	if err := json.Unmarshal(d.Body, &e); err != nil {
		log.Printf("discarding unparsable message: %v", err)
		_ = d.Nack(false, false) // malformed: retrying cannot help
		return
	}
	if err := e.Validate(); err != nil {
		log.Printf("discarding invalid event %s: %v", e.ID, err)
		_ = d.Nack(false, false)
		return
	}

	if err := c.store.RecordEvent(ctx, e); err != nil {
		log.Printf("database write failed for event %s, requeueing: %v", e.ID, err)
		_ = d.Nack(false, true)
		return
	}
	if err := d.Ack(false); err != nil {
		log.Printf("ack failed for event %s: %v", e.ID, err)
	}
}

// connect dials the broker and declares the exchange, queue and binding.
func (c *Consumer) connect() (*amqp.Connection, *amqp.Channel, error) {
	var err error
	for i := 0; i < 30; i++ {
		var conn *amqp.Connection
		conn, err = amqp.Dial(c.url)
		if err == nil {
			var ch *amqp.Channel
			if ch, err = conn.Channel(); err == nil {
				if err = c.declare(ch); err == nil {
					log.Println("connected to rabbitmq")
					return conn, ch, nil
				}
				_ = ch.Close()
			}
			_ = conn.Close()
		}
		log.Printf("waiting for rabbitmq: %v", err)
		time.Sleep(2 * time.Second)
	}
	return nil, nil, fmt.Errorf("rabbitmq not reachable: %w", err)
}

func (c *Consumer) declare(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(c.exchange, "fanout", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(c.queue, true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(c.queue, "", c.exchange, false, nil); err != nil {
		return err
	}
	// Keep a small prefetch: events are cheap to process and this bounds the
	// number of messages that must be redelivered after a crash.
	return ch.Qos(10, 0, false)
}
