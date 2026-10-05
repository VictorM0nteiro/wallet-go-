package postgres

import (
	"context"
	"testing"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
)

// seeder is what the transfer tests need from a strategy: the port, plus a
// direct Execute to seed balances without going through the service.
type seeder interface {
	app.TransferExecutor
	Execute(ctx context.Context, req app.TransferRequest) error
}

type strategy struct {
	name string
	new  func(*Pool) seeder
}

var strategies = []strategy{
	{name: "locking", new: func(p *Pool) seeder { return NewLockingTransferExecutor(p) }},
	{name: "serializable", new: func(p *Pool) seeder { return NewSerializableTransferExecutor(p) }},
}

// forEachStrategy runs one test body against every concurrency strategy, so
// both are held to the same contract.
func forEachStrategy(t *testing.T, body func(t *testing.T, mk func(*Pool) seeder)) {
	t.Helper()
	for _, s := range strategies {
		t.Run(s.name, func(t *testing.T) { body(t, s.new) })
	}
}
