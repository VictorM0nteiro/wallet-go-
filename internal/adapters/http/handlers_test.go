package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/app"
	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

type fakeCash struct {
	got app.CashCommand
	res app.TransferResult
	err error
}

func (f *fakeCash) Deposit(_ context.Context, c app.CashCommand) (app.TransferResult, error) {
	f.got = c
	return f.res, f.err
}

func (f *fakeCash) Withdraw(_ context.Context, c app.CashCommand) (app.TransferResult, error) {
	f.got = c
	return f.res, f.err
}

type fakeTransfers struct{}

func (fakeTransfers) Transfer(context.Context, app.TransferCommand) (app.TransferResult, error) {
	return app.TransferResult{}, nil
}

type fakeAccounts struct {
	gotAfter int64
	gotLimit int
}

func (fakeAccounts) Create(_ context.Context, ownerID string) (app.Account, error) {
	return app.Account{ID: uuid.New(), OwnerID: ownerID, Kind: domain.AccountKindCustomer, Currency: "BRL"}, nil
}

func (fakeAccounts) Balance(context.Context, uuid.UUID) (domain.Money, error) {
	return domain.NewMoney(1234), nil
}

func (f *fakeAccounts) Entries(_ context.Context, _ uuid.UUID, after int64, limit int) (app.EntriesPage, error) {
	f.gotAfter, f.gotLimit = after, limit
	return app.EntriesPage{}, nil
}

func newTestHandler(cash cashService, accounts accountService) http.Handler {
	return NewRouter(Deps{
		Accounts:       accounts,
		Cash:           cash,
		Transfers:      fakeTransfers{},
		APIKey:         "secret",
		RequestTimeout: 2 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func doRequest(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-API-Key", "secret")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var er errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return er.Error.Code
}

func TestAPIKeyIsRequired(t *testing.T) {
	h := newTestHandler(&fakeCash{}, &fakeAccounts{})
	rec := doRequest(h, http.MethodGet, "/accounts/"+uuid.NewString()+"/balance", "", map[string]string{"X-API-Key": ""})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequestIDIsGeneratedAndPreserved(t *testing.T) {
	h := newTestHandler(&fakeCash{}, &fakeAccounts{})
	path := "/accounts/" + uuid.NewString() + "/balance"

	if got := doRequest(h, http.MethodGet, path, "", nil).Header().Get("X-Request-ID"); got == "" {
		t.Error("expected a generated X-Request-ID")
	}
	got := doRequest(h, http.MethodGet, path, "", map[string]string{"X-Request-ID": "abc-123"}).Header().Get("X-Request-ID")
	if got != "abc-123" {
		t.Errorf("X-Request-ID = %q, want abc-123", got)
	}
}

func TestDeposit_BuildsScopedCommandAndReplaysStoredResponse(t *testing.T) {
	accountID := uuid.New()
	cash := &fakeCash{res: app.TransferResult{
		Replayed: true,
		Response: app.StoredResponse{StatusCode: http.StatusCreated, Body: []byte(`{"transfer_id":"abc"}`)},
	}}
	h := newTestHandler(cash, &fakeAccounts{})
	body := `{"amount_cents":5000}`

	rec := doRequest(h, http.MethodPost, "/accounts/"+accountID.String()+"/deposits", body, map[string]string{"Idempotency-Key": "k-1"})

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"transfer_id":"abc"}` {
		t.Fatalf("response = %d %q, want the stored 201 body", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("expected Idempotent-Replayed: true on a replay")
	}

	wantFingerprint, _ := app.Fingerprint([]byte(body))
	got := cash.got
	if got.Fingerprint != wantFingerprint || got.AccountID != accountID || got.AmountCents != 5000 {
		t.Errorf("command = %+v", got)
	}
	if got.Key.Key != "k-1" || got.Key.Scope == "" || got.Key.Endpoint != "POST /accounts/"+accountID.String()+"/deposits" {
		t.Errorf("key = %+v", got.Key)
	}
}

func TestDeposit_ErrorMapping(t *testing.T) {
	validPath := "/accounts/" + uuid.NewString() + "/deposits"
	withKey := map[string]string{"Idempotency-Key": "k-1"}

	tests := []struct {
		name       string
		path       string
		body       string
		headers    map[string]string
		svcErr     error
		wantStatus int
		wantCode   string
	}{
		{"sem_idempotency_key", validPath, `{"amount_cents":1}`, nil, nil, 400, "bad_request"},
		{"campo_desconhecido", validPath, `{"amount_cents":1,"x":2}`, withKey, nil, 400, "bad_request"},
		{"json_invalido", validPath, `{`, withKey, nil, 400, "bad_request"},
		{"valor_decimal", validPath, `{"amount_cents":10.5}`, withKey, nil, 400, "bad_request"},
		{"id_invalido", "/accounts/nao-e-uuid/deposits", `{"amount_cents":1}`, withKey, nil, 400, "bad_request"},
		{"chave_reusada", validPath, `{"amount_cents":1}`, withKey, app.ErrIdempotencyKeyReuse, 422, "idempotency_key_reuse"},
		{"requisicao_em_voo", validPath, `{"amount_cents":1}`, withKey, app.ErrRequestInFlight, 409, "request_in_flight"},
		{"conta_inexistente", validPath, `{"amount_cents":1}`, withKey, domain.ErrAccountNotFound, 404, "account_not_found"},
		{"erro_interno_nao_vaza", validPath, `{"amount_cents":1}`, withKey, errors.New("pq: secret detail"), 500, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(&fakeCash{err: tt.svcErr}, &fakeAccounts{})
			rec := doRequest(h, http.MethodPost, tt.path, tt.body, tt.headers)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := errorCode(t, rec); got != tt.wantCode {
				t.Errorf("code = %q, want %q", got, tt.wantCode)
			}
			if strings.Contains(rec.Body.String(), "secret detail") {
				t.Error("internal error details leaked to the client")
			}
		})
	}
}

func TestEntries_QueryParameters(t *testing.T) {
	path := "/accounts/" + uuid.NewString() + "/entries"

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantAfter  int64
		wantLimit  int
	}{
		{"padroes", "", 200, 0, 50},
		{"cursor_e_limite", "?after=42&limit=10", 200, 42, 10},
		{"limite_zero", "?limit=0", 400, 0, 0},
		{"limite_acima_do_maximo", "?limit=201", 400, 0, 0},
		{"limite_nao_numerico", "?limit=abc", 400, 0, 0},
		{"cursor_negativo", "?after=-1", 400, 0, 0},
		{"cursor_nao_numerico", "?after=abc", 400, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accounts := &fakeAccounts{}
			h := newTestHandler(&fakeCash{}, accounts)

			rec := doRequest(h, http.MethodGet, path+tt.query, "", nil)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == 200 && (accounts.gotAfter != tt.wantAfter || accounts.gotLimit != tt.wantLimit) {
				t.Errorf("after/limit = %d/%d, want %d/%d", accounts.gotAfter, accounts.gotLimit, tt.wantAfter, tt.wantLimit)
			}
		})
	}
}
