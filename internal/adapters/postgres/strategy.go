package postgres

import (
	"fmt"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
)

// NewTransferExecutor picks the concurrency strategy by name. The empty name
// means the default, locking.
func NewTransferExecutor(strategy string, pool *Pool) (app.TransferExecutor, error) {
	switch strategy {
	case "", "locking":
		return NewLockingTransferExecutor(pool), nil
	case "serializable":
		return NewSerializableTransferExecutor(pool), nil
	default:
		return nil, fmt.Errorf("postgres: unknown transfer strategy %q (want locking or serializable)", strategy)
	}
}

// O que faz: centraliza a escolha. O main não precisa saber qual implementação existe, só o nome. Uma estratégia nova exige uma linha aqui e nada mais.
