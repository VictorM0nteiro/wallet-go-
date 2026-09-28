package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthz_NeverDependsOnTheDatabase(t *testing.T) {
	// healthz is not even wired to a Pinger: this test documents that
	// on purpose, so a future change can't accidentally make it depend on one.
	h := newTestHandler(&fakeCash{}, &fakeAccounts{})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
	}{
		{"database reachable", nil, http.StatusOK},
		{"database unreachable", errors.New("boom"), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewRouter(Deps{
				Accounts:       &fakeAccounts{},
				Cash:           &fakeCash{},
				Transfers:      fakeTransfers{},
				DB:             fakePinger{err: tt.pingErr},
				APIKey:         "secret",
				RequestTimeout: testRequestTimeout,
				Logger:         testLogger(),
			})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestHealthEndpoints_DoNotRequireAPIKey(t *testing.T) {
	h := NewRouter(Deps{
		Accounts:       &fakeAccounts{},
		Cash:           &fakeCash{},
		Transfers:      fakeTransfers{},
		DB:             fakePinger{},
		APIKey:         "secret",
		RequestTimeout: testRequestTimeout,
		Logger:         testLogger(),
	})
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s required an API key", path)
		}
	}
}
