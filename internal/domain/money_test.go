package domain

import (
	"errors"
	"math"
	"testing"
)

func TestNewTransferAmount(t *testing.T) {
	tests := []struct {
		name    string
		cents   int64
		wantErr error
	}{
		{"valor_positivo_e_aceito", 5000, nil},
		{"zero_e_rejeitado", 0, ErrInvalidAmount},
		{"negativo_e_rejeitado", -1, ErrInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewTransferAmount(tt.cents)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got.Cents() != tt.cents {
				t.Fatalf("Cents() = %d, want %d", got.Cents(), tt.cents)
			}
		})
	}
}

func TestMoney_AddSub(t *testing.T) {
	a := NewMoney(300)
	b := NewMoney(200)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: unexpected error: %v", err)
	}
	if sum.Cents() != 500 {
		t.Fatalf("Add() = %d, want 500", sum.Cents())
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub: unexpected error: %v", err)
	}
	if diff.Cents() != 100 {
		t.Fatalf("Sub() = %d, want 100", diff.Cents())
	}
}

func TestMoney_Add_OverflowsAtMaxInt64(t *testing.T) {
	max := NewMoney(math.MaxInt64)
	_, err := max.Add(NewMoney(1))
	if !errors.Is(err, ErrMoneyOverflow) {
		t.Fatalf("err = %v, want %v", err, ErrMoneyOverflow)
	}
}

func TestMoney_String(t *testing.T) {
	tests := []struct {
		cents int64
		want  string
	}{
		{0, "R$ 0,00"},
		{1, "R$ 0,01"},
		{99, "R$ 0,99"},
		{100, "R$ 1,00"},
		{9700, "R$ 97,00"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := NewMoney(tt.cents).String()
			if got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
