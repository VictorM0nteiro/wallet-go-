CREATE TABLE outbox (
    event_id        UUID PRIMARY KEY,
    event_type      TEXT NOT NULL,
    payload         JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index: the only query the relay ever runs is "find pending work",
-- and this keeps that cheap no matter how many published rows accumulate
-- (retention/cleanup is an accepted, undecided-until-measured cost -- see
-- docs/adr/0007-outbox-relay-no-broker.md).
CREATE INDEX outbox_pending_idx ON outbox (next_attempt_at) WHERE published_at IS NULL;

-- wallet_app only ever appends events, in the same transaction as the
-- transfer they describe (X8 least privilege, extended).
GRANT INSERT ON outbox TO wallet_app;

-- wallet_relay only ever reads pending rows and marks its own progress on
-- them -- it never touches accounts/transfers/entries/idempotency_keys.
GRANT SELECT, UPDATE (published_at, attempts, next_attempt_at) ON outbox TO wallet_relay;
