package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// idemFixture creates a system account and two customers, funds ana with
// 10000 cents, and returns a service wired to the locking strategy.
func idemFixture(t *testing.T) (*Pool, *app.TransferService, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := newTestPool(t)
	accounts := NewAccountRepository(pool)
	exec := NewLockingTransferExecutor(pool)

	system := newTestAccount(t, accounts, "system", domain.AccountKindSystem)
	ana := newTestAccount(t, accounts, "ana", domain.AccountKindCustomer)
	bruno := newTestAccount(t, accounts, "bruno", domain.AccountKindCustomer)
	mustTransfer(t, exec, system, ana, 10000)

	return pool, app.NewTransferService(exec), ana, bruno
}

func cmdFor(t *testing.T, scope, key string, from, to uuid.UUID, cents int64) app.TransferCommand {
	t.Helper()
	body := fmt.Sprintf(`{"from":%q,"to":%q,"amount_cents":%d}`, from, to, cents)
	fp, err := app.Fingerprint([]byte(body))
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return app.TransferCommand{
		Key:           app.IdempotencyKey{Scope: scope, Endpoint: "POST /transfers", Key: key},
		Fingerprint:   fp,
		FromAccountID: from,
		ToAccountID:   to,
		AmountCents:   cents,
	}
}

func TestTransferService_ConcurrentSameKeyTransfersExactlyOnce(t *testing.T) {
	pool, svc, ana, bruno := idemFixture(t)
	transfersBefore, entriesBefore := countTable(t, pool, "transfers"), countTable(t, pool, "entries")
	cmd := cmdFor(t, "api-key-1", "k-1", ana, bruno, 3000)

	const n = 10
	results := make([]app.TransferResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Transfer(context.Background(), cmd)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()

	if got := countTable(t, pool, "transfers") - transfersBefore; got != 1 {
		t.Errorf("transfers created = %d, want exactly 1", got)
	}
	if got := countTable(t, pool, "entries") - entriesBefore; got != 2 {
		t.Errorf("entries created = %d, want exactly 2", got)
	}

	replays := 0
	for i, res := range results {
		if res.TransferID != results[0].TransferID {
			t.Errorf("request %d returned transfer %s, want %s", i, res.TransferID, results[0].TransferID)
		}
		if res.Replayed {
			replays++
		}
	}
	if replays != n-1 {
		t.Errorf("replays = %d, want %d", replays, n-1)
	}

	if got := mustBalance(t, NewEntryRepository(pool), ana); got != 7000 {
		t.Errorf("ana = %d, want 7000", got)
	}
	assertInvariants(t, pool)
}

func TestTransferService_ReplayReturnsOriginalStatusAndBody(t *testing.T) {
	_, svc, ana, bruno := idemFixture(t)
	cmd := cmdFor(t, "api-key-1", "k-1", ana, bruno, 3000)

	first, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.Replayed || !second.Replayed {
		t.Fatalf("replayed = %v/%v, want false/true", first.Replayed, second.Replayed)
	}
	if second.Response.StatusCode != first.Response.StatusCode {
		t.Errorf("status = %d, want %d", second.Response.StatusCode, first.Response.StatusCode)
	}

	var body struct {
		TransferID uuid.UUID `json:"transfer_id"`
	}
	if err := json.Unmarshal(second.Response.Body, &body); err != nil {
		t.Fatalf("decode replayed body %q: %v", second.Response.Body, err)
	}
	if body.TransferID != first.TransferID {
		t.Errorf("body transfer_id = %s, want %s", body.TransferID, first.TransferID)
	}
}

func TestTransferService_SameKeyDifferentBodyIsRejected(t *testing.T) {
	pool, svc, ana, bruno := idemFixture(t)

	if _, err := svc.Transfer(context.Background(), cmdFor(t, "api-key-1", "k-1", ana, bruno, 3000)); err != nil {
		t.Fatalf("first: %v", err)
	}
	transfersBefore := countTable(t, pool, "transfers")

	_, err := svc.Transfer(context.Background(), cmdFor(t, "api-key-1", "k-1", ana, bruno, 4000))
	if !errors.Is(err, app.ErrIdempotencyKeyReuse) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyReuse", err)
	}
	if got := countTable(t, pool, "transfers"); got != transfersBefore {
		t.Errorf("transfers = %d, want %d", got, transfersBefore)
	}
}

// The in_flight state is never visible to other transactions when the claim
// and the transfer share one transaction (a concurrent claim blocks instead).
// This seeds the row directly to cover the 409 branch of the use case.
func TestTransferService_InFlightKeyIsRejected(t *testing.T) {
	pool, svc, ana, bruno := idemFixture(t)
	cmd := cmdFor(t, "api-key-1", "k-1", ana, bruno, 3000)

	const seed = `
              INSERT INTO idempotency_keys (scope, endpoint, key, request_fingerprint, state)
              VALUES ($1, $2, $3, $4, 'in_flight')
      `
	if _, err := pool.Exec(context.Background(), seed, cmd.Key.Scope, cmd.Key.Endpoint, cmd.Key.Key, cmd.Fingerprint); err != nil {
		t.Fatalf("seed in_flight key: %v", err)
	}
	transfersBefore := countTable(t, pool, "transfers")

	_, err := svc.Transfer(context.Background(), cmd)
	if !errors.Is(err, app.ErrRequestInFlight) {
		t.Fatalf("err = %v, want ErrRequestInFlight", err)
	}
	if got := countTable(t, pool, "transfers"); got != transfersBefore {
		t.Errorf("transfers = %d, want %d", got, transfersBefore)
	}
}

func TestTransferService_SameKeyInDifferentScopesDoesNotCollide(t *testing.T) {
	pool, svc, ana, bruno := idemFixture(t)
	transfersBefore := countTable(t, pool, "transfers")

	for _, scope := range []string{"api-key-1", "api-key-2"} {
		res, err := svc.Transfer(context.Background(), cmdFor(t, scope, "same-key", ana, bruno, 1000))
		if err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
		if res.Replayed {
			t.Errorf("scope %s: request was replayed, keys must be scoped", scope)
		}
	}
	if got := countTable(t, pool, "transfers") - transfersBefore; got != 2 {
		t.Errorf("transfers created = %d, want 2", got)
	}
	assertInvariants(t, pool)
}
