REVOKE ALL ON outbox FROM wallet_relay;
REVOKE ALL ON outbox FROM wallet_app;
DROP TABLE IF EXISTS outbox;
