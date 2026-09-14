CREATE TABLE accounts (
    id          UUID PRIMARY KEY,
    owner_id    TEXT NOT NULL,
    kind        TEXT NOT NULL,          -- 'customer' | 'system'
    currency    CHAR(3) NOT NULL DEFAULT 'BRL',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_id, kind, currency)
);

CREATE TABLE transfers (
    id               UUID PRIMARY KEY,
    from_account_id  UUID NOT NULL REFERENCES accounts(id),
    to_account_id    UUID NOT NULL REFERENCES accounts(id),
    amount_cents     BIGINT NOT NULL CHECK (amount_cents > 0),
    status           TEXT   NOT NULL,   -- 'completed' | 'rejected'
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_account_id <> to_account_id)
);

CREATE TABLE entries (
    id           BIGSERIAL PRIMARY KEY,
    transfer_id  UUID   NOT NULL REFERENCES transfers(id),
    account_id   UUID   NOT NULL REFERENCES accounts(id),
    amount_cents BIGINT NOT NULL CHECK (amount_cents <> 0),  -- + entra, − sai
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX entries_account_idx ON entries (account_id, id);

CREATE TABLE idempotency_keys (
    scope                TEXT NOT NULL,      -- dono da chave
    endpoint             TEXT NOT NULL,
    key                  TEXT NOT NULL,
    request_fingerprint  TEXT NOT NULL,      -- sha256 do corpo canonicalizado
    state                TEXT NOT NULL,      -- 'in_flight' | 'completed'
    status_code          INT,
    response_body        JSONB,
    transfer_id          UUID REFERENCES transfers(id),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, endpoint, key)
);