package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/VictorM0nteiro/wallet-go/internal/domain"
)

type fakeAccountStore struct {
	accounts map[uuid.UUID]Account
}

func (f *fakeAccountStore) Create(_ context.Context, a Account) error {
	f.accounts[a.ID] = a
	return nil
}

func (f *fakeAccountStore) FindByID(_ context.Context, id uuid.UUID) (Account, error) {
	a, ok := f.accounts[id]
	if !ok {
		return Account{}, domain.ErrAccountNotFound
	}
	return a, nil
}

type fakeLedger struct {
	rows []LedgerEntry
}

func (f *fakeLedger) SumByAccount(_ context.Context, _ uuid.UUID) (domain.Money, error) {
	return 0, nil
}

func (f *fakeLedger) ListByAccount(_ context.Context, _ uuid.UUID, afterID int64, limit int) ([]LedgerEntry, error) {
	var out []LedgerEntry
	for _, r := range f.rows {
		if r.ID > afterID && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func newAccountService(rows ...LedgerEntry) (*AccountService, uuid.UUID) {
	store := &fakeAccountStore{accounts: map[uuid.UUID]Account{}}
	svc := NewAccountService(store, &fakeLedger{rows: rows})
	acc, _ := svc.Create(context.Background(), "ana")
	return svc, acc.ID
}

func TestAccountService_Create_RejectsEmptyOwner(t *testing.T) {
	svc, _ := newAccountService()
	if _, err := svc.Create(context.Background(), "   "); !errors.Is(err, ErrInvalidOwner) {
		t.Fatalf("err = %v, want ErrInvalidOwner", err)
	}
}

func TestAccountService_Balance_UnknownAccount(t *testing.T) {
	svc, _ := newAccountService()
	if _, err := svc.Balance(context.Background(), uuid.New()); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}

func TestAccountService_Entries_Pagination(t *testing.T) {
	rows := []LedgerEntry{{ID: 1}, {ID: 2}, {ID: 3}}

	tests := []struct {
		name     string
		after    int64
		limit    int
		wantIDs  []int64
		wantNext *int64
	}{
		{"mais_paginas_devolve_cursor", 0, 2, []int64{1, 2}, ptr(2)},
		{"ultima_pagina_sem_cursor", 2, 2, []int64{3}, nil},
		{"limite_exato_sem_cursor", 0, 3, []int64{1, 2, 3}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, id := newAccountService(rows...)
			page, err := svc.Entries(context.Background(), id, tt.after, tt.limit)
			if err != nil {
				t.Fatalf("Entries: %v", err)
			}
			if len(page.Entries) != len(tt.wantIDs) {
				t.Fatalf("entries = %d, want %d", len(page.Entries), len(tt.wantIDs))
			}
			for i, e := range page.Entries {
				if e.ID != tt.wantIDs[i] {
					t.Errorf("entry %d id = %d, want %d", i, e.ID, tt.wantIDs[i])
				}
			}
			switch {
			case tt.wantNext == nil && page.NextAfter != nil:
				t.Errorf("NextAfter = %d, want nil", *page.NextAfter)
			case tt.wantNext != nil && (page.NextAfter == nil || *page.NextAfter != *tt.wantNext):
				t.Errorf("NextAfter = %v, want %d", page.NextAfter, *tt.wantNext)
			}
		})
	}
}

func ptr(v int64) *int64 { return &v }
