package app

import "errors"

// ErrConcurrencyConflict is returned by a retrying strategy (serializable)
// that gave up: the transaction kept losing races with others touching the
// same rows. Nothing was committed, so the same request — same
// Idempotency-Key included — is safe to retry.
var ErrConcurrencyConflict = errors.New("app: too many concurrent conflicts, retry the request")
