package postgres

import (
	"context"
	"testing"
)

// assertInvariants checks the two ledger invariants from
// docs/escopo-carteira-go.md §3. Both queries must return zero rows.
func assertInvariants(t *testing.T, pool *Pool) {
	t.Helper()

	const everyTransferSumsToZero = `
              SELECT transfer_id, SUM(amount_cents) FROM entries
              GROUP BY transfer_id HAVING SUM(amount_cents) <> 0
      `
	const noCustomerGoesNegative = `
              SELECT e.account_id, SUM(e.amount_cents) FROM entries e
              JOIN accounts a ON a.id = e.account_id
              WHERE a.kind = 'customer'
              GROUP BY e.account_id HAVING SUM(e.amount_cents) < 0
      `

	if n := countRows(t, pool, everyTransferSumsToZero); n != 0 {
		t.Errorf("invariant 1 violated: %d transfer(s) do not sum to zero", n)
	}
	if n := countRows(t, pool, noCustomerGoesNegative); n != 0 {
		t.Errorf("invariant 2 violated: %d customer account(s) have a negative balance", n)
	}
}

func countRows(t *testing.T, pool *Pool, query string) int {
	t.Helper()

	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	return n
}
