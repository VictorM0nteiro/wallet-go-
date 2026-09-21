package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	// ErrIdempotencyKeyReuse is returned when a key is reused with a
	// different request body (HTTP 422).
	ErrIdempotencyKeyReuse = errors.New("app: idempotency key reused with a different request body")

	// ErrRequestInFlight is returned when a request with the same key is
	// still being processed (HTTP 409).
	ErrRequestInFlight = errors.New("app: a request with this idempotency key is still in flight")
)

// States of an idempotency key row.
const (
	KeyStateInFlight  = "in_flight"
	KeyStateCompleted = "completed"
)

func Fingerprint(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("app: fingerprint: %w", err)
	}
	if dec.More() {
		return "", errors.New("app: fingerprint: trailing data after JSON body")
	}

	// json.Marshal writes map keys in sorted order, which is the canonical form.
	canonical, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("app: fingerprint: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
