package postgres

import (
	"time"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
	"github.com/google/uuid"
)

// Account is the row shape of the accounts table. It is a persistence
// concern, not a domain entity — internal/domain has no Account type yet
// (a deliberate Bloco 4 decision), so this struct only borrows
// domain.AccountKind for the Kind field.
type Account struct {
      ID        uuid.UUID
      OwnerID   string
      Kind      domain.AccountKind
      Currency  string
      CreatedAt time.Time
}