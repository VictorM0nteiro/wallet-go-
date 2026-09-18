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
)
