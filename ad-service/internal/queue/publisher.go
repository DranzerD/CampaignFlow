package queue

import (
	"context"
	"encoding/json"
	"errors"
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

// confirmTimeout bounds how long Publish waits for the broker to ack a
// message before treating it as failed and logging it as dropped.
const confirmTimeout = 5 * time.Second

// Publisher pushes events to RabbitMQ from a background goroutine so that
// HTTP handlers never block on the broker.
type Publisher struct {
	exchange string
	queue    string
	url      string
	events   chan Event
	done     chan struct{}
	conn     *amqp.Connection
	channel  *amqp.Channel
	confirms chan amqp.Confirmation
}

// NewPublisher connects, declares the exchange/queue/binding and starts the
// writer loop.
//
// queue is declared here too, not just by the Analytics consumer, so that
// whichever service starts first creates the topology. A fanout exchange
// with no bound queue silently discards every message published to it, so if
// only the consumer declared the queue, ads served before Analytics has
// started and bound it would publish impressions into a queue that does not
// exist yet -- and RabbitMQ would drop them with no error.
func NewPublisher(url, exchange, queue string) (*Publisher, error) {
	p := &Publisher{
		exchange: exchange,
		queue:    queue,
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
				if err = p.declare(ch); err == nil {
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

// declare sets up the same topology as the Analytics consumer (see
// analytics-service/internal/queue/consumer.go) and puts the channel into
// publisher-confirm mode, so a broker-side rejection is observable instead of
// silently assumed to have succeeded.
func (p *Publisher) declare(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(p.exchange, "fanout", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(p.queue, true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(p.queue, "", p.exchange, false, nil); err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		return err
	}
	p.confirms = ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	return nil
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

// publishOnce sends one event and waits for the broker's publisher-confirm
// before reporting success. Without waiting for the confirm, a message that
// the broker rejects (for example because it could not be routed or
// persisted) would be indistinguishable from one that succeeded -- the
// channel write itself never fails.
func (p *Publisher) publishOnce(e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), confirmTimeout)
	defer cancel()
	if err := p.channel.PublishWithContext(ctx, p.exchange, "", false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    e.ID,
			Body:         body,
		}); err != nil {
		return err
	}

	select {
	case confirm, ok := <-p.confirms:
		if !ok {
			return errors.New("confirmation channel closed")
		}
		if !confirm.Ack {
			return fmt.Errorf("broker nacked event %s", e.ID)
		}
		return nil
	case <-time.After(confirmTimeout):
		return fmt.Errorf("timed out waiting for broker confirmation of event %s", e.ID)
	}
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
