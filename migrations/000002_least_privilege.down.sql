REVOKE ALL ON accounts, transfers, entries, idempotency_keys FROM wallet_app;
REVOKE UPDATE (currency) ON accounts FROM wallet_app;
REVOKE USAGE ON SEQUENCE entries_id_seq FROM wallet_app;