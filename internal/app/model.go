package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// idempotency
// IdempotencyKey identifies a key. It is scoped by (Scope, Endpoint, Key),
// never global: two owners may use the same string without colliding.
type IdempotencyKey struct {
	Scope    string
	Endpoint string
	Key      string
}

// StoredResponse is what a replay must return: the original status and body.
type StoredResponse struct {
	StatusCode int
	Body       []byte
}

// StoredKey is a persisted idempotency key.
type StoredKey struct {
	Fingerprint string
	State       string
	TransferID  [16]byte
	Response    StoredResponse
}

// transfer
// TransferRequest is a validated request to move Amount between accounts.
type TransferRequest struct {
	ID            uuid.UUID
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	Amount        domain.Money
}

// TransferTx is what the use case may do inside one database transaction.
// The claim, the transfer and the stored response commit or roll back
// together — that is what makes the idempotency guarantee hold.
type TransferTx interface {
	// ClaimKey tries to register the key as in_flight. It reports false if
	// the key already exists.
	ClaimKey(ctx context.Context, key IdempotencyKey, fingerprint string) (claimed bool, err error)
	LoadKey(ctx context.Context, key IdempotencyKey) (StoredKey, error)
	// Execute moves the money. How it stays correct under concurrency is
	// the adapter's strategy, invisible to the use case.
	Execute(ctx context.Context, req TransferRequest) error
	CompleteKey(ctx context.Context, key IdempotencyKey, transferID uuid.UUID, resp StoredResponse) error
}

// TransferExecutor is the transfer port: run fn atomically in a transaction.
// fn may be called more than once (retry strategies), so it must not leak
// side effects outside the transaction.
type TransferExecutor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TransferTx) error) error
}

// TransferCommand is one transfer request as received from the outside.
type TransferCommand struct {
	Key           IdempotencyKey
	Fingerprint   string
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	AmountCents   int64
}

// TransferResult is the outcome of Transfer.
type TransferResult struct {
	TransferID uuid.UUID
	Response   StoredResponse
	// Replayed is true when the response came from a previous request
	// with the same key.
	Replayed bool
}

// TransferService is the transfer use case.
type TransferService struct {
	executor TransferExecutor
}

// NewTransferService builds a TransferService backed by executor.
func NewTransferService(executor TransferExecutor) *TransferService {
	return &TransferService{executor: executor}
}
