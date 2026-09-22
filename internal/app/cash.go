package app

import (
	"context"

	"github.com/google/uuid"
)

// NewCashService builds a CashService.
func NewCashService(transfers *TransferService, systemAccountID uuid.UUID) *CashService {
	return &CashService{transfers: transfers, systemAccountID: systemAccountID}
}

// Deposit moves money from the system account into accountID.
func (s *CashService) Deposit(ctx context.Context, cmd CashCommand) (TransferResult, error) {
	return s.transfers.Transfer(ctx, TransferCommand{
		Key:           cmd.Key,
		Fingerprint:   cmd.Fingerprint,
		FromAccountID: s.systemAccountID,
		ToAccountID:   cmd.AccountID,
		AmountCents:   cmd.AmountCents,
	})
}

// Withdraw moves money from accountID into the system account.
func (s *CashService) Withdraw(ctx context.Context, cmd CashCommand) (TransferResult, error) {
	return s.transfers.Transfer(ctx, TransferCommand{
		Key:           cmd.Key,
		Fingerprint:   cmd.Fingerprint,
		FromAccountID: cmd.AccountID,
		ToAccountID:   s.systemAccountID,
		AmountCents:   cmd.AmountCents,
	})
}
