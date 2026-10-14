-- Dev-only password, same convention as 01-app-role.sql. Runs only when the
-- data directory is empty, so a fresh volume gets the role before migration
-- 000003 grants it anything.
CREATE ROLE wallet_relay LOGIN PASSWORD 'wallet_relay_dev';
