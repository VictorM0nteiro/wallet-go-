package domain

import "errors"

var (
	// ErrInvalidAmount is returned when a transfer/deposit/withdrawal
	// amount is zero or negative.
	ErrInvalidAmount = errors.New("domain: amount must be greater than zero")

	// ErrInsufficientFunds is returned when a debit would take a
	// non-system account's balance below zero.
	ErrInsufficientFunds = errors.New("domain: insufficient funds")

	// ErrMoneyOverflow is returned when a Money arithmetic operation would
	// overflow int64.
	ErrMoneyOverflow = errors.New("domain: money operation overflows int64")

	// ErrSameAccount is returned when a transfer's source and destination
	// are the same account.
	ErrSameAccount = errors.New("domain: source and destination accounts must differ")

	// ErrAccountNotFound is returned when a transfer references an account
	// that does not exist.
	ErrAccountNotFound = errors.New("domain: account not found")
)
