package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Event is one row of the outbox: something that happened, recorded in the
// same transaction as whatever caused it, for a relay to deliver later.
type Event struct {
	ID        uuid.UUID
	Type      string
	Payload   []byte
	CreatedAt time.Time
	Attempts  int
}

// Publisher delivers an event somewhere outside this process. This project
// ships no real destination on purpose — "no broker" is part of its thesis
// (docs/adr/0007-outbox-relay-no-broker.md, confirmed at the dependency
// level: nothing redis/rabbitmq/kafka/nats/amqp-shaped is in go.sum).
// LogPublisher below is a stand-in; a real deployment would swap it for a
// webhook call, a notification service, whatever the actual destination is.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// LogPublisher logs the event instead of delivering it anywhere real.
type LogPublisher struct {
	Logger *slog.Logger
}

func (p LogPublisher) Publish(_ context.Context, e Event) error {
	p.Logger.Info("relay: publishing event", "event_id", e.ID, "event_type", e.Type)
	return nil
}

// AppendEvent records a domain event in the same transaction as whatever
// l.tx is already doing — the same property ClaimKey/CompleteKey already
// rely on for the idempotency guarantee. The event's id is generated here,
// once, and never regenerated: a relay retrying a delivery reuses it.
func (l *lockingTx) AppendEvent(ctx context.Context, eventType string, payload []byte) error {
	const query = `INSERT INTO outbox (event_id, event_type, payload) VALUES ($1, $2, $3)`
	if _, err := l.tx.Exec(ctx, query, uuid.New(), eventType, payload); err != nil {
		return fmt.Errorf("postgres: append event: %w", err)
	}
	return nil
}
