package domain

import (
	"errors"
	"testing"
)

func TestCanDebit(t *testing.T) {
	tests := []struct {
		name    string
		balance int64
		amount  int64
		kind    AccountKind
		wantErr error
	}{
		{
			name:    "saldo_exatamente_igual_ao_valor_permite_debito",
			balance: 5000,
			amount:  5000,
			kind:    AccountKindCustomer,
			wantErr: nil,
		},
		{
			name:    "saldo_um_centavo_menor_que_o_valor_rejeita",
			balance: 4999,
			amount:  5000,
			kind:    AccountKindCustomer,
			wantErr: ErrInsufficientFunds,
		},
		{
			name:    "conta_system_pode_ficar_negativa",
			balance: 0,
			amount:  1_000_000_00,
			kind:    AccountKindSystem,
			wantErr: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CanDebit(NewMoney(tt.balance), NewMoney(tt.amount), tt.kind)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CanDebit() err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
