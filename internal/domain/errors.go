package domain

import "errors"

var (
	ErrInvalidAmount     = errors.New("domain: amount must be greater than zero")
	ErrInsufficientFunds = errors.New("domain: Insufficient funds")
	ErrMoneyOverflow     = errors.New("domain : money operation overflows int64")
)
