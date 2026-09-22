package httpapi

import (
	"time"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
)

// Money always travels as the integer amount_cents, never as a string or float.

type createAccountRequest struct {
	OwnerID string `json:"owner_id"`
}

type amountRequest struct {
	AmountCents int64 `json:"amount_cents"`
}

type transferRequest struct {
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	AmountCents   int64  `json:"amount_cents"`
}

type accountResponse struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	Kind      string    `json:"kind"`
	Currency  string    `json:"currency"`
	CreatedAt time.Time `json:"created_at"`
}

type balanceResponse struct {
	AccountID    string `json:"account_id"`
	BalanceCents int64  `json:"balance_cents"`
}

type entryResponse struct {
	ID          int64     `json:"id"`
	TransferID  string    `json:"transfer_id"`
	AmountCents int64     `json:"amount_cents"`
	CreatedAt   time.Time `json:"created_at"`
}

type entriesResponse struct {
	Entries   []entryResponse `json:"entries"`
	NextAfter *int64          `json:"next_after,omitempty"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

func toAccountResponse(a app.Account) accountResponse {
	return accountResponse{
		ID:        a.ID.String(),
		OwnerID:   a.OwnerID,
		Kind:      string(a.Kind),
		Currency:  a.Currency,
		CreatedAt: a.CreatedAt,
	}
}

func toEntriesResponse(p app.EntriesPage) entriesResponse {
	out := entriesResponse{Entries: make([]entryResponse, 0, len(p.Entries)), NextAfter: p.NextAfter}
	for _, e := range p.Entries {
		out.Entries = append(out.Entries, entryResponse{
			ID:          e.ID,
			TransferID:  e.TransferID.String(),
			AmountCents: e.AmountCents,
			CreatedAt:   e.CreatedAt,
		})
	}
	return out
}
