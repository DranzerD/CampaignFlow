package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Event is the message contract shared with the Analytics Service.
type Event struct {
	ID         string    `json:"id"`
	AdID       string    `json:"ad_id"`
	CampaignID string    `json:"campaign_id"`
	EventType  string    `json:"event_type"`
	Timestamp  time.Time `json:"timestamp"`
}

const (
	EventImpression = "impression"
	EventClick      = "click"
)

// Publisher pushes events to RabbitMQ from a background goroutine so that
// HTTP handlers never block on the broker.
type Publisher struct {
	exchange string
	url      string
	events   chan Event
	done     chan struct{}
	conn     *amqp.Connection
	channel  *amqp.Channel
}

// NewPublisher connects, declares the exchange and starts the writer loop.
func NewPublisher(url, exchange string) (*Publisher, error) {
	p := &Publisher{
		exchange: exchange,
		url:      url,
		events:   make(chan Event, 1024),
		done:     make(chan struct{}),
	}
	if err := p.connect(); err != nil {
		return nil, err
	}
	go p.loop()
	return p, nil
}

func (p *Publisher) connect() error {
	var err error
	for i := 0; i < 30; i++ {
		var conn *amqp.Connection
		conn, err = amqp.Dial(p.url)
		if err == nil {
			var ch *amqp.Channel
			ch, err = conn.Channel()
			if err == nil {
				err = ch.ExchangeDeclare(p.exchange, "fanout", true, false, false, false, nil)
				if err == nil {
					p.conn, p.channel = conn, ch
					log.Println("connected to rabbitmq")
					return nil
				}
				_ = ch.Close()
			}
			_ = conn.Close()
		}
		log.Printf("waiting for rabbitmq: %v", err)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("rabbitmq not reachable: %w", err)
}

// Publish queues an event. It never blocks: if the buffer is full the event is
// dropped with a log line rather than slowing down ad serving.
func (p *Publisher) Publish(e Event) {
	select {
	case p.events <- e:
	default:
		log.Printf("event buffer full, dropping %s event for ad %s", e.EventType, e.AdID)
	}
}

func (p *Publisher) loop() {
	defer close(p.done)
	for e := range p.events {
		if err := p.publishOnce(e); err != nil {
			log.Printf("publish failed (%v), reconnecting", err)
			if err := p.connect(); err != nil {
				log.Printf("reconnect failed: %v", err)
				continue
			}
			if err := p.publishOnce(e); err != nil {
				log.Printf("dropping %s event %s: %v", e.EventType, e.ID, err)
			}
		}
	}
}

func (p *Publisher) publishOnce(e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.channel.PublishWithContext(ctx, p.exchange, "", false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    e.ID,
			Body:         body,
		})
}

// Close drains the buffer and shuts the connection down.
func (p *Publisher) Close() {
	close(p.events)
	<-p.done
	if p.channel != nil {
		_ = p.channel.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
}
