package domain

type AccountKind string

// AccountKind distinguishes ordinary customer accounts from the single
// `system` account used as the counterparty for deposits and withdrawals.
const (
	AccountKindCostume AccountKind = "customer"
	AccountKindSystem  AccountKind = "System"
)

// CanDebit reports whether amount can be debited from an account currently
// holding balance, given its kind. The non-negative-balance invariant is
// enforced here: a debit that would take balance below zero is rejected.
//
// The system account is the sole exception. Deposits and withdrawals are
// modeled as transfers to/from it, so it must be allowed to go negative —
// otherwise the very first deposit in the system would be impossible, since
// there would be nowhere for the offsetting debit to come from.
func CanDebit(balance Money, amount Money, kind AccountKind) error{
	if kind == AccountKindSystem{
		return nil
	}
	remaining, err := balance.Sub(amount)
	if err != nil{
		return err
	}
	if remaining < 0 {
		return ErrInsufficientFunds
	}
	return nil
}