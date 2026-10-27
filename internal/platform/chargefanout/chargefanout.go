// Package chargefanout is the RabbitMQ charge fan-out: publish one message per ACTIVE billing
// profile, and a listener consumes per profile.
// It is the multi-pod alternative to billingjob's in-process Charge loop: the cron fires
// Publish (work-list → N messages); any pod's Consumer drains the queue, charging one profile
// per message so a slow/failing profile can't stall the rest and work spreads across pods.
//
// Gated behind PROXCLOUD_JOBS_RABBIT_FANOUT (default off → in-process Charge). The charge math is
// unchanged (the consumer calls billingjob.ChargeProfileByID = the same per-profile body).
package chargefanout

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Queue is the charge work queue.
const Queue = "proxcloud.charge"

// Msg is one unit of charge work: charge ProfileID for the given TimeUnit.
type Msg struct {
	ProfileID string `json:"profileId"`
	TimeUnit  string `json:"timeUnit"`
}

// Publisher publishes a message body to a queue (satisfied by *amqp.Client).
type Publisher interface {
	Publish(ctx context.Context, queue string, body []byte) error
}

// Consumer subscribes a handler to a queue, returning a stop func (satisfied by *amqp.Client).
type Consumer interface {
	Consume(queue string, handler func(body []byte) error) (func() error, error)
}

// Charger charges one profile (satisfied by *billingjob.Service).
type Charger interface {
	ChargeProfileByID(ctx context.Context, profileID, timeUnit string, now time.Time) error
}

// ProfileLister returns the ACTIVE profile ids to fan out (satisfied by *billingjob.Service).
type ProfileLister interface {
	ActiveProfileIDs(ctx context.Context) ([]string, error)
}

// Publish fans out one charge message per ACTIVE profile. Returns the count published.
func Publish(ctx context.Context, pub Publisher, lister ProfileLister, timeUnit string) (int, error) {
	ids, err := lister.ActiveProfileIDs(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		body, err := json.Marshal(Msg{ProfileID: id, TimeUnit: timeUnit})
		if err != nil {
			return 0, err
		}
		if err := pub.Publish(ctx, Queue, body); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// StartConsumer subscribes to the charge queue and charges one profile per message. The
// returned stop func unsubscribes (call on shutdown).
func StartConsumer(con Consumer, charger Charger, log *slog.Logger) (func() error, error) {
	if log == nil {
		log = slog.Default()
	}
	return con.Consume(Queue, func(body []byte) error {
		var m Msg
		if err := json.Unmarshal(body, &m); err != nil {
			log.Error("chargefanout: bad message", "err", err)
			return err
		}
		if err := charger.ChargeProfileByID(context.Background(), m.ProfileID, m.TimeUnit, time.Now().UTC()); err != nil {
			log.Error("chargefanout: charge profile", "profileId", m.ProfileID, "timeUnit", m.TimeUnit, "err", err)
			return err
		}
		return nil
	})
}
