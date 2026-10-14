package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

func suiteSuccessfulTransferAppendsExactlyOneEvent(t *testing.T, mk func(*Pool) seeder) {
	pool, svc, ana, bruno := idemFixture(t, mk)
	outboxBefore := countTable(t, pool, "outbox")
	cmd := cmdFor(t, "api-key-1", "k-1", ana, bruno, 3000)

	res, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if got := countTable(t, pool, "outbox") - outboxBefore; got != 1 {
		t.Fatalf("outbox rows created = %d, want 1", got)
	}

	var eventType string
	var payload []byte
	const q = `SELECT event_type, payload FROM outbox ORDER BY created_at DESC LIMIT 1`
	if err := pool.QueryRow(context.Background(), q).Scan(&eventType, &payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if eventType != "transfer.completed" {
		t.Errorf("event_type = %q, want transfer.completed", eventType)
	}
	var body struct {
		TransferID  string `json:"transfer_id"`
		AmountCents int64  `json:"amount_cents"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode payload %q: %v", payload, err)
	}
	if body.TransferID != res.TransferID.String() || body.AmountCents != 3000 {
		t.Errorf("payload = %+v, does not match the transfer it describes", body)
	}
}

// Same "all or nothing" guarantee entries already have, extended to the
// event: a rolled-back transfer must leave no trace in the outbox either.
func suiteFailedTransferAppendsNoEvent(t *testing.T, mk func(*Pool) seeder) {
	pool, svc, ana, bruno := idemFixture(t, mk)
	outboxBefore := countTable(t, pool, "outbox")
	cmd := cmdFor(t, "api-key-1", "k-1", ana, bruno, 999999)

	_, err := svc.Transfer(context.Background(), cmd)
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if got := countTable(t, pool, "outbox"); got != outboxBefore {
		t.Errorf("outbox rows = %d, want %d", got, outboxBefore)
	}
}

func suiteReplayDoesNotAppendASecondEvent(t *testing.T, mk func(*Pool) seeder) {
	pool, svc, ana, bruno := idemFixture(t, mk)
	cmd := cmdFor(t, "api-key-1", "k-1", ana, bruno, 3000)

	if _, err := svc.Transfer(context.Background(), cmd); err != nil {
		t.Fatalf("first: %v", err)
	}
	outboxAfterFirst := countTable(t, pool, "outbox")

	res, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !res.Replayed {
		t.Fatal("second request must be a replay")
	}
	if got := countTable(t, pool, "outbox"); got != outboxAfterFirst {
		t.Errorf("outbox rows = %d, want %d (a replay must not append again)", got, outboxAfterFirst)
	}
}

func TestOutbox_SuccessfulTransferAppendsExactlyOneEvent(t *testing.T) {
	forEachStrategy(t, suiteSuccessfulTransferAppendsExactlyOneEvent)
}

func TestOutbox_FailedTransferAppendsNoEvent(t *testing.T) {
	forEachStrategy(t, suiteFailedTransferAppendsNoEvent)
}

func TestOutbox_ReplayDoesNotAppendASecondEvent(t *testing.T) {
	forEachStrategy(t, suiteReplayDoesNotAppendASecondEvent)
}
