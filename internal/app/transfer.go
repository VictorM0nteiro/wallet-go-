package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// TransferRequest is a validated request to move Amount from one account
// to another. ID is the identity of the transfer being created.
type TransferRequest struct {
	ID             uuid.UUID
	FromAccountID uuid.UUID
	ToAccountID    uuid.UUID
	Amount         domain.Money
}

// TransferExecutor is the port for atomically executing a transfer. How it
// guarantees atomicity and the non-negative-balance invariant under
// concurrency (row locks, SERIALIZABLE + retry, ...) is the adapter's
// business — the use case must not know which strategy is in use.
type TransferExecutor interface {
	Execute(ctx context.Context, req TransferRequest) error
}

// TransferService is the transfer use case.
type TransferService struct {
	executor TransferExecutor
}

// NewTransferService builds a TransferService backed by executor.
func NewTransferService(executor TransferExecutor) *TransferService {
	return &TransferService{executor: executor}
}

// Transfer validates the input and delegates execution to the port. It
// returns the id of the created transfer.
func (s *TransferService) Transfer(ctx context.Context, from, to uuid.UUID, amountCents int64) (uuid.UUID, error) {
	amount, err := domain.NewTransferAmount(amountCents)
	if err != nil {
		return uuid.Nil, err
	}
	if from == to {
		return uuid.Nil, domain.ErrSameAccount
	}

	req := TransferRequest{
		ID:            uuid.New(),
		FromAccountID: from,
		ToAccountID:   to,
		Amount:        amount,
	}
	if err := s.executor.Execute(ctx, req); err != nil {
		return uuid.Nil, err
	}
	return req.ID, nil
}
