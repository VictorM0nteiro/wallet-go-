package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

const (
	maxBodyBytes   = 1 << 20
	maxKeyLength   = 255
	defaultLimit   = 50
	maxEntriesPage = 200
)

// The handlers depend on these narrow interfaces, satisfied by the app
// services, so they can be tested with fakes.
type accountService interface {
	Create(ctx context.Context, ownerID string) (app.Account, error)
	Balance(ctx context.Context, accountID uuid.UUID) (domain.Money, error)
	Entries(ctx context.Context, accountID uuid.UUID, afterID int64, limit int) (app.EntriesPage, error)
}

type cashService interface {
	Deposit(ctx context.Context, cmd app.CashCommand) (app.TransferResult, error)
	Withdraw(ctx context.Context, cmd app.CashCommand) (app.TransferResult, error)
}

type transferService interface {
	Transfer(ctx context.Context, cmd app.TransferCommand) (app.TransferResult, error)
}

// Handler holds the HTTP handlers of the API.
type Handler struct {
	accounts  accountService
	cash      cashService
	transfers transferService
	scope     string
	logger    *slog.Logger
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var req createAccountRequest
	if err := decodeStrict(body, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	acc, err := h.accounts.Create(r.Context(), req.OwnerID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAccountResponse(acc))
}

func (h *Handler) deposit(w http.ResponseWriter, r *http.Request) {
	h.cashOp(w, r, "deposits", h.cash.Deposit)
}

func (h *Handler) withdraw(w http.ResponseWriter, r *http.Request) {
	h.cashOp(w, r, "withdrawals", h.cash.Withdraw)
}

func (h *Handler) cashOp(w http.ResponseWriter, r *http.Request, op string, run func(context.Context, app.CashCommand) (app.TransferResult, error)) {
	accountID, err := parseAccountID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var req amountRequest
	if err := decodeStrict(body, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	fingerprint, err := app.Fingerprint(body)
	if err != nil {
		h.writeError(w, r, badRequest(err.Error()))
		return
	}
	// The account id is part of the endpoint so the same key and body on a
	// different account is a different request, not a replay.
	key, err := h.idempotencyKey(r, fmt.Sprintf("POST /accounts/%s/%s", accountID, op))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	res, err := run(r.Context(), app.CashCommand{
		Key:         key,
		Fingerprint: fingerprint,
		AccountID:   accountID,
		AmountCents: req.AmountCents,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeResult(w, res)
}

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var req transferRequest
	if err := decodeStrict(body, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	from, err := uuid.Parse(req.FromAccountID)
	if err != nil {
		h.writeError(w, r, badRequest("from_account_id must be a valid UUID"))
		return
	}
	to, err := uuid.Parse(req.ToAccountID)
	if err != nil {
		h.writeError(w, r, badRequest("to_account_id must be a valid UUID"))
		return
	}
	fingerprint, err := app.Fingerprint(body)
	if err != nil {
		h.writeError(w, r, badRequest(err.Error()))
		return
	}
	key, err := h.idempotencyKey(r, "POST /transfers")
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	res, err := h.transfers.Transfer(r.Context(), app.TransferCommand{
		Key:           key,
		Fingerprint:   fingerprint,
		FromAccountID: from,
		ToAccountID:   to,
		AmountCents:   req.AmountCents,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeResult(w, res)
}

func (h *Handler) balance(w http.ResponseWriter, r *http.Request) {
	accountID, err := parseAccountID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	bal, err := h.accounts.Balance(r.Context(), accountID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResponse{AccountID: accountID.String(), BalanceCents: bal.Cents()})
}

func (h *Handler) entries(w http.ResponseWriter, r *http.Request) {
	accountID, err := parseAccountID(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	after, err := queryAfter(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	page, err := h.accounts.Entries(r.Context(), accountID, after, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toEntriesResponse(page))
}

func (h *Handler) idempotencyKey(r *http.Request, endpoint string) (app.IdempotencyKey, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return app.IdempotencyKey{}, badRequest("Idempotency-Key header is required")
	}
	if len(key) > maxKeyLength {
		return app.IdempotencyKey{}, badRequest("Idempotency-Key must be at most 255 characters")
	}
	return app.IdempotencyKey{Scope: h.scope, Endpoint: endpoint, Key: key}, nil
}

func parseAccountID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, badRequest("account id must be a valid UUID")
	}
	return id, nil
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, badRequest("could not read request body (max 1 MiB)")
	}
	return body, nil
}

// decodeStrict rejects unknown fields and trailing data, so a typo in a
// field name is an error instead of a silently ignored value.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: " + err.Error())
	}
	if dec.More() {
		return badRequest("invalid JSON body: unexpected trailing data")
	}
	return nil
}

func queryAfter(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, badRequest("after must be a non-negative integer entry id")
	}
	return v, nil
}

func queryLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 || v > maxEntriesPage {
		return 0, badRequest(fmt.Sprintf("limit must be an integer between 1 and %d", maxEntriesPage))
	}
	return v, nil
}
