GRANT SELECT, INSERT ON accounts, transfers, entries TO wallet_app;
GRANT SELECT, INSERT ON idempotency_keys TO wallet_app;
GRANT UPDATE (state, status_code, response_body, transfer_id) ON idempotency_keys TO wallet_app;

-- SELECT ... FOR UPDATE needs UPDATE privilege. One harmless column is enough
-- for the locking strategy; the trade-off is discussed below.
GRANT UPDATE (currency) ON accounts TO wallet_app;

GRANT USAGE ON SEQUENCE entries_id_seq TO wallet_app;