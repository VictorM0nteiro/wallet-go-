-- Dev-only password. In a real environment this comes from a secret, not
-- from a file in version control. Runs only when the data directory is
-- empty (docker-entrypoint-initdb.d semantics), so a fresh volume gets the
-- role before migration 000002 grants it anything.
CREATE ROLE wallet_app LOGIN PASSWORD 'wallet_app_dev';
