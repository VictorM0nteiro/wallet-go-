package app

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// fakeExecutor is an in-memory TransferExecutor that also plays the role of
// the TransferTx, with rollback on error.
type fakeExecutor struct {
	keys     map[IdempotencyKey]StoredKey
	executed []TransferRequest
	execErr  error
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{keys: map[IdempotencyKey]StoredKey{}}
}

func (f *fakeExecutor) InTx(ctx context.Context, fn func(context.Context, TransferTx) error) error {
	keysBefore := maps.Clone(f.keys)
	executedBefore := len(f.executed)
	if err := fn(ctx, f); err != nil {
		f.keys = keysBefore
		f.executed = f.executed[:executedBefore]
		return err
	}
	return nil
}

func (f *fakeExecutor) ClaimKey(_ context.Context, key IdempotencyKey, fingerprint string) (bool, error) {
	if _, exists := f.keys[key]; exists {
		return false, nil
	}
	f.keys[key] = StoredKey{Fingerprint: fingerprint, State: KeyStateInFlight}
	return true, nil
}

func (f *fakeExecutor) LoadKey(_ context.Context, key IdempotencyKey) (StoredKey, error) {
	return f.keys[key], nil
}

func (f *fakeExecutor) Execute(_ context.Context, req TransferRequest) error {
	if f.execErr != nil {
		return f.execErr
	}
	f.executed = append(f.executed, req)
	return nil
}

func (f *fakeExecutor) CompleteKey(_ context.Context, key IdempotencyKey, transferID uuid.UUID, resp StoredResponse) error {
	stored := f.keys[key]
	stored.State = KeyStateCompleted
	stored.TransferID = transferID
	stored.Response = resp
	f.keys[key] = stored
	return nil
}

func newCommand(fingerprint string, cents int64) TransferCommand {
	return TransferCommand{
		Key:           IdempotencyKey{Scope: "s", Endpoint: "POST /transfers", Key: "k1"},
		Fingerprint:   fingerprint,
		FromAccountID: uuid.New(),
		ToAccountID:   uuid.New(),
		AmountCents:   cents,
	}
}

func TestTransferService_NewKeyExecutesAndStoresResponse(t *testing.T) {
	exec := newFakeExecutor()
	svc := NewTransferService(exec)
	cmd := newCommand("fp-a", 5000)

	res, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if res.Replayed {
		t.Error("first request must not be a replay")
	}
	if res.Response.StatusCode != StatusTransferCreated {
		t.Errorf("status = %d, want %d", res.Response.StatusCode, StatusTransferCreated)
	}
	if len(exec.executed) != 1 || exec.executed[0].ID != res.TransferID {
		t.Fatalf("executed = %v, want exactly the returned transfer", exec.executed)
	}
	if exec.keys[cmd.Key].State != KeyStateCompleted {
		t.Errorf("key state = %q, want completed", exec.keys[cmd.Key].State)
	}
}

func TestTransferService_ReplayReturnsStoredResponseWithoutExecutingAgain(t *testing.T) {
	exec := newFakeExecutor()
	svc := NewTransferService(exec)
	cmd := newCommand("fp-a", 5000)

	first, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if !second.Replayed {
		t.Error("second request must be a replay")
	}
	if second.TransferID != first.TransferID {
		t.Errorf("replay transfer id = %s, want %s", second.TransferID, first.TransferID)
	}
	if second.Response.StatusCode != first.Response.StatusCode ||
		string(second.Response.Body) != string(first.Response.Body) {
		t.Errorf("replay response = %+v, want %+v", second.Response, first.Response)
	}
	if len(exec.executed) != 1 {
		t.Errorf("executed %d times, want 1", len(exec.executed))
	}
}

func TestTransferService_SameKeyDifferentFingerprintIsRejected(t *testing.T) {
	exec := newFakeExecutor()
	svc := NewTransferService(exec)
	cmd := newCommand("fp-a", 5000)

	if _, err := svc.Transfer(context.Background(), cmd); err != nil {
		t.Fatalf("first: %v", err)
	}
	other := cmd
	other.Fingerprint = "fp-b"

	_, err := svc.Transfer(context.Background(), other)
	if !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyReuse", err)
	}
	if len(exec.executed) != 1 {
		t.Errorf("executed %d times, want 1", len(exec.executed))
	}
}

func TestTransferService_InFlightKeyIsRejected(t *testing.T) {
	exec := newFakeExecutor()
	svc := NewTransferService(exec)
	cmd := newCommand("fp-a", 5000)
	exec.keys[cmd.Key] = StoredKey{Fingerprint: "fp-a", State: KeyStateInFlight}

	_, err := svc.Transfer(context.Background(), cmd)
	if !errors.Is(err, ErrRequestInFlight) {
		t.Fatalf("err = %v, want ErrRequestInFlight", err)
	}
	if len(exec.executed) != 0 {
		t.Errorf("executed %d times, want 0", len(exec.executed))
	}
}

func TestTransferService_InvalidInputDoesNotTouchIdempotencyStore(t *testing.T) {
	same := uuid.New()
	tests := []struct {
		name    string
		mutate  func(*TransferCommand)
		wantErr error
	}{
		{"valor_zero", func(c *TransferCommand) { c.AmountCents = 0 }, domain.ErrInvalidAmount},
		{"valor_negativo", func(c *TransferCommand) { c.AmountCents = -1 }, domain.ErrInvalidAmount},
		{"mesma_conta", func(c *TransferCommand) { c.FromAccountID, c.ToAccountID = same, same }, domain.ErrSameAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newFakeExecutor()
			svc := NewTransferService(exec)
			cmd := newCommand("fp-a", 5000)
			tt.mutate(&cmd)

			_, err := svc.Transfer(context.Background(), cmd)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(exec.keys) != 0 {
				t.Errorf("invalid input consumed an idempotency key: %v", exec.keys)
			}
		})
	}
}

func TestTransferService_FailedTransferReleasesTheKeyForRetry(t *testing.T) {
	exec := newFakeExecutor()
	svc := NewTransferService(exec)
	cmd := newCommand("fp-a", 5000)

	exec.execErr = domain.ErrInsufficientFunds
	if _, err := svc.Transfer(context.Background(), cmd); !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if len(exec.keys) != 0 {
		t.Fatalf("failed transfer left a key behind: %v", exec.keys)
	}

	exec.execErr = nil
	res, err := svc.Transfer(context.Background(), cmd)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Replayed {
		t.Error("retry after a failure must execute, not replay")
	}
}

func TestFingerprint(t *testing.T) {
	base := `{"from":"a","to":"b","amount_cents":5000}`
	tests := []struct {
		name     string
		other    string
		wantSame bool
	}{
		{"ordem_dos_campos_nao_importa", `{"amount_cents":5000,"to":"b","from":"a"}`, true},
		{"espacos_nao_importam", "{ \"from\": \"a\",\n \"to\": \"b\", \"amount_cents\": 5000 }", true},
		{"valor_diferente_muda_o_fingerprint", `{"from":"a","to":"b","amount_cents":5001}`, false},
		{"campo_extra_muda_o_fingerprint", `{"from":"a","to":"b","amount_cents":5000,"x":1}`, false},
	}
	want, err := Fingerprint([]byte(base))
	if err != nil {
		t.Fatalf("fingerprint base: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Fingerprint([]byte(tt.other))
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			if (got == want) != tt.wantSame {
				t.Errorf("same = %v, want %v", got == want, tt.wantSame)
			}
		})
	}
}

func TestFingerprint_RejectsInvalidBodies(t *testing.T) {
	for _, body := range []string{"", "not json", `{"a":1} {"b":2}`} {
		if _, err := Fingerprint([]byte(body)); err == nil {
			t.Errorf("Fingerprint(%q) = nil error, want error", body)
		}
	}
}
