// Package amqp wraps the RabbitMQ connection (the ProxCloud message broker) and the
// publish/consume primitives the charge fan-out uses (one message per billing
// profile → any pod consumes → per-profile failure isolation).
package amqp

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

type Client struct {
	conn *amqp.Connection
}

// Connect dials RabbitMQ.
func Connect(host string, port int, username, password string) (*Client, error) {
	url := fmt.Sprintf("amqp://%s:%s@%s:%d/", username, password, host, port)
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq dial: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Healthy reports whether the connection is open (used by readiness).
func (c *Client) Healthy() bool {
	return c.conn != nil && !c.conn.IsClosed()
}

func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Publish declares a durable queue and publishes a persistent message to it via the
// default exchange (routing key == queue name, the AMQP default-exchange convention).
// A fresh channel per call keeps it simple + safe for the low-rate cron fan-out.
func (c *Client) Publish(ctx context.Context, queue string, body []byte) error {
	ch, err := c.conn.Channel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		return err
	}
	return ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Body:         body,
	})
}

// Consume declares the queue and starts a manual-ack consumer: handler runs per message;
// success → ack, error → nack (requeue=false, so a poison message is dropped not looped,
// logged and skipped rather than redelivered forever). Prefetch 1 spreads work across pods.
// Returns a stop func (closes the channel) — call it on shutdown.
func (c *Client) Consume(queue string, handler func(body []byte) error) (func() error, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return nil, err
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		_ = ch.Close()
		return nil, err
	}
	if err := ch.Qos(1, 0, false); err != nil {
		_ = ch.Close()
		return nil, err
	}
	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		_ = ch.Close()
		return nil, err
	}
	go func() {
		for d := range deliveries {
			if err := handler(d.Body); err != nil {
				_ = d.Nack(false, false)
			} else {
				_ = d.Ack(false)
			}
		}
	}()
	return ch.Close, nil
}
