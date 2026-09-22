package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

func TestStatusFor(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"entrada_malformada", badRequest("x"), http.StatusBadRequest, "bad_request"},
		{"conta_inexistente", domain.ErrAccountNotFound, http.StatusNotFound, "account_not_found"},
		{"conta_duplicada", domain.ErrAccountAlreadyExists, http.StatusConflict, "account_already_exists"},
		{"requisicao_em_voo", app.ErrRequestInFlight, http.StatusConflict, "request_in_flight"},
		{"chave_com_corpo_diferente", app.ErrIdempotencyKeyReuse, http.StatusUnprocessableEntity, "idempotency_key_reuse"},
		{"saldo_insuficiente", domain.ErrInsufficientFunds, http.StatusUnprocessableEntity, "insufficient_funds"},
		{"valor_invalido", domain.ErrInvalidAmount, http.StatusUnprocessableEntity, "invalid_amount"},
		{"mesma_conta", domain.ErrSameAccount, http.StatusUnprocessableEntity, "same_account"},
		{"overflow", domain.ErrMoneyOverflow, http.StatusUnprocessableEntity, "amount_overflow"},
		{"dono_invalido", app.ErrInvalidOwner, http.StatusUnprocessableEntity, "invalid_owner"},
		{"timeout_vira_503", context.DeadlineExceeded, http.StatusServiceUnavailable, "service_unavailable"},
		{"erro_embrulhado_ainda_e_reconhecido", fmt.Errorf("postgres: x: %w", domain.ErrInsufficientFunds), http.StatusUnprocessableEntity, "insufficient_funds"},
		{"erro_desconhecido_vira_500", errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code := statusFor(tt.err)
			if status != tt.wantStatus || code != tt.wantCode {
				t.Fatalf("statusFor = (%d, %q), want (%d, %q)", status, code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}
