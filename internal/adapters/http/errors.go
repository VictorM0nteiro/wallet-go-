package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

// badRequestError marks malformed input (bad JSON, missing header, bad id).
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func badRequest(msg string) error { return &badRequestError{msg: msg} }

// statusFor translates an error into an HTTP status and a stable machine
// readable code. Anything unknown is a 500: internals never leak to clients.
func statusFor(err error) (int, string) {
	var bad *badRequestError
	switch {
	case errors.As(err, &bad):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, domain.ErrAccountNotFound):
		return http.StatusNotFound, "account_not_found"
	case errors.Is(err, domain.ErrAccountAlreadyExists):
		return http.StatusConflict, "account_already_exists"
	case errors.Is(err, app.ErrRequestInFlight):
		return http.StatusConflict, "request_in_flight"
	case errors.Is(err, app.ErrIdempotencyKeyReuse):
		return http.StatusUnprocessableEntity, "idempotency_key_reuse"
	case errors.Is(err, domain.ErrInsufficientFunds):
		return http.StatusUnprocessableEntity, "insufficient_funds"
	case errors.Is(err, domain.ErrInvalidAmount):
		return http.StatusUnprocessableEntity, "invalid_amount"
	case errors.Is(err, domain.ErrSameAccount):
		return http.StatusUnprocessableEntity, "same_account"
	case errors.Is(err, domain.ErrMoneyOverflow):
		return http.StatusUnprocessableEntity, "amount_overflow"
	case errors.Is(err, app.ErrInvalidOwner):
		return http.StatusUnprocessableEntity, "invalid_owner"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "service_unavailable"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}
