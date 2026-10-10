package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	apiKeyScopeKey
)

const maxRequestIDLength = 128

// apiKeyScope is which of the two keys apiKeyAuth matched. There is no
// "unauthenticated" value here on purpose: requireWriteScope only runs after
// apiKeyAuth already accepted the request.
type apiKeyScope int

const (
	scopeWrite apiKeyScope = iota
	scopeRead
)

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestID makes sure every request carries a correlation id: the caller's
// X-Request-ID if present and sane, otherwise a fresh one. It is echoed in
// the response and logged with every line about the request.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > maxRequestIDLength {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// accessLog writes one structured line per request.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r)
			logger.Info("request",
				"request_id", requestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// recoverer turns a panic in a handler into a logged 500 instead of a
// dropped connection. It must sit inside accessLog so the 500 is logged.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				logger.Error("panic recovered",
					"request_id", requestIDFrom(r.Context()),
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				writeJSON(w, http.StatusInternalServerError, errorResponse{
					Error: errorBody{Code: "internal_error", Message: http.StatusText(http.StatusInternalServerError)},
				})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// timeout bounds the whole request. The context reaches the queries, so a
// slow database surfaces as context.DeadlineExceeded (mapped to 503).
func timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// apiKeyAuth accepts the write key (full access) and, if readKey is not
// empty, a second read-only key. Which one matched is stashed in the
// request context as an apiKeyScope, for requireWriteScope to check later
// — apiKeyAuth itself only decides "is this key valid at all".
func apiKeyAuth(writeKey, readKey string) func(http.Handler) http.Handler {
	wantWrite := []byte(writeKey)
	wantRead := []byte(readKey)
	hasReadKey := readKey != ""
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("X-API-Key"))
			var scope apiKeyScope
			switch {
			case subtle.ConstantTimeCompare(got, wantWrite) == 1:
				scope = scopeWrite
			case hasReadKey && subtle.ConstantTimeCompare(got, wantRead) == 1:
				scope = scopeRead
			default:
				writeJSON(w, http.StatusUnauthorized, errorResponse{
					Error: errorBody{Code: "unauthorized", Message: "missing or invalid API key"},
				})
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiKeyScopeKey, scope)))
		})
	}
}

// requireWriteScope sits in front of every money-moving or account-creating
// route. A read-only key already passed apiKeyAuth — the credential is
// valid — but is rejected here with 403, not 401: it just cannot do this.
func requireWriteScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope, _ := r.Context().Value(apiKeyScopeKey).(apiKeyScope)
		if scope != scopeWrite {
			writeJSON(w, http.StatusForbidden, errorResponse{
				Error: errorBody{Code: "forbidden", Message: "this API key is read-only"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
