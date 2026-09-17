package domain

import (
	"fmt"
	"math"
)

// Money represents an amount of BRL in integer cents. It is the only
// representation of currency in the domain and never float64, in any layer,
// in any log, in any JSON response.
type Money int64

// NewMoney wraps a raw cent value with no sign constraint. Use it for
// balances and ledger entries, which are allowed to be negative (debit
// entries, and any entry on the `system` account).
func NewMoney(cents int64) Money {
	return Money(cents)
}

// NewTransferAmount validates a cent value meant to be moved by a deposit,
// withdrawal or transfer request. Unlike NewMoney, zero and negative values
// are rejected here: `transfers.amount_cents > 0` and
// `entries.amount_cents <> 0` are enforced at the DB layer, and the domain
// rejects the same thing earlier, with a typed error instead of a DB error.
func NewTransferAmount(cents int64) (Money, error) {
	if cents <= 0 {
		return 0, ErrInvalidAmount
	}
	return Money(cents), nil
}

// Cents returns the raw integer cent value.
func (m Money) Cents() int64 {
	return int64(m)
}

// Add returns m + other, or ErrMoneyOverflow if the result overflows int64.
func (m Money) Add(other Money) (Money, error) {
	a, b := int64(m), int64(other)
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, ErrMoneyOverflow
	}
	return Money(sum), nil
}

// Sub returns m - other, or ErrMoneyOverflow if the result overflows int64.
func (m Money) Sub(other Money) (Money, error) {
	if other == math.MinInt64 {
		return 0, ErrMoneyOverflow
	}
	return m.Add(-other)
}

// String formats cents as BRL, e.g. Money(9700).String() == "R$ 97,00".
// This is presentation only JSON encoding of Money must stay the raw
// integer field `amount_cents`, never this string.
func (m Money) String() string {
	cents := int64(m)
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%sR$ %d,%02d", sign, cents/100, cents%100)
}
