package domain

import "testing"

// These benchmark the domain's hot path: every deposit, withdrawal and
// transfer calls NewTransferAmount once and CanDebit once, pure arithmetic,
// no I/O. The point (Session 9 of the execution plan) is to show that this
// layer contributes nothing measurable to a transfer's latency — the cost
// lives entirely in the database round trip and, under contention, the row
// lock. See BenchmarkLockingTransfer_HotContention in the postgres package
// for that side, profiled with pprof.
//
//	go test ./internal/domain/ -bench . -benchmem

func BenchmarkCanDebit(b *testing.B) {
	balance := NewMoney(1_000_000_00)
	amount := NewMoney(100)
	for b.Loop() {
		if err := CanDebit(balance, amount, AccountKindCustomer); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCanDebit_SystemAccount(b *testing.B) {
	balance := NewMoney(-1_000_000_00)
	amount := NewMoney(100)
	for b.Loop() {
		if err := CanDebit(balance, amount, AccountKindSystem); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewTransferAmount(b *testing.B) {
	for b.Loop() {
		if _, err := NewTransferAmount(100); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMoney_Add(b *testing.B) {
	a, c := NewMoney(100), NewMoney(200)
	for b.Loop() {
		if _, err := a.Add(c); err != nil {
			b.Fatal(err)
		}
	}
}
