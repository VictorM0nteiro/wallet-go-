package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

type fakeExecutor struct {
	calls []TransferRequest
	err   error
}

func (f *fakeExecutor) Execute(_ context.Context, req TransferRequest) error {
	f.calls = append(f.calls, req)
	return f.err
}

func TestTransferService_Transfer(t *testing.T) {
	from, to := uuid.New(), uuid.New()
	boom := errors.New("boom")

	tests := []struct {
		name      string
		from, to  uuid.UUID
		cents     int64
		execErr   error
		wantErr   error
		wantCalls int
	}{
		{"transferencia_valida_chama_o_executor", from, to, 5000, nil, nil, 1},
		{"valor_zero_e_rejeitado_sem_chamar_o_executor", from, to, 0, nil, domain.ErrInvalidAmount, 0},
		{"valor_negativo_e_rejeitado_sem_chamar_o_executor", from, to, -1, nil, domain.ErrInvalidAmount, 0},
		{"mesma_conta_e_rejeitada_sem_chamar_o_executor", from, from, 5000, nil, domain.ErrSameAccount, 0},
		{"erro_do_executor_e_propagado", from, to, 5000, boom, boom, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExecutor{err: tt.execErr}
			svc := NewTransferService(exec)

			id, err := svc.Transfer(context.Background(), tt.from, tt.to, tt.cents)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(exec.calls) != tt.wantCalls {
				t.Fatalf("executor calls = %d, want %d", len(exec.calls), tt.wantCalls)
			}
			if tt.wantErr == nil && id == uuid.Nil {
				t.Fatal("expected a transfer id, got uuid.Nil")
			}
		})
	}
}
