package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// StatusTransferCreated is the status stored with a successful transfer and
// replayed verbatim on duplicate requests.
const StatusTransferCreated = 201

// Transfer validates the input, then runs the idempotent flow: claim the
// key; if it was already claimed, replay or reject; otherwise execute the
// transfer and store the response, all in one transaction.
func (s *TransferService) Transfer(ctx context.Context, cmd TransferCommand) (TransferResult, error) {
	amount, err := domain.NewTransferAmount(cmd.AmountCents)
	if err != nil {
		return TransferResult{}, err
	}
	if cmd.FromAccountID == cmd.ToAccountID {
		return TransferResult{}, domain.ErrSameAccount
	}

	var result TransferResult
	err = s.executor.InTx(ctx, func(ctx context.Context, tx TransferTx) error {
		result = TransferResult{}

		claimed, err := tx.ClaimKey(ctx, cmd.Key, cmd.Fingerprint)
		if err != nil {
			return err
		}
		if !claimed {
			result, err = replay(ctx, tx, cmd)
			return err
		}

		req := TransferRequest{
			ID:            uuid.New(),
			FromAccountID: cmd.FromAccountID,
			ToAccountID:   cmd.ToAccountID,
			Amount:        amount,
		}
		if err := tx.Execute(ctx, req); err != nil {
			return err
		}

		body, err := json.Marshal(struct {
			TransferID uuid.UUID `json:"transfer_id"`
		}{req.ID})
		if err != nil {
			return fmt.Errorf("app: encode response: %w", err)
		}
		resp := StoredResponse{StatusCode: StatusTransferCreated, Body: body}

		if err := tx.CompleteKey(ctx, cmd.Key, req.ID, resp); err != nil {
			return err
		}
		result = TransferResult{TransferID: req.ID, Response: resp}
		return nil
	})
	if err != nil {
		return TransferResult{}, err
	}
	return result, nil
}

func replay(ctx context.Context, tx TransferTx, cmd TransferCommand) (TransferResult, error) {
	stored, err := tx.LoadKey(ctx, cmd.Key)
	if err != nil {
		return TransferResult{}, err
	}
	if stored.Fingerprint != cmd.Fingerprint {
		return TransferResult{}, ErrIdempotencyKeyReuse
	}
	if stored.State != KeyStateCompleted {
		return TransferResult{}, ErrRequestInFlight
	}
	return TransferResult{
		TransferID: stored.TransferID,
		Response:   stored.Response,
		Replayed:   true,
	}, nil
}
