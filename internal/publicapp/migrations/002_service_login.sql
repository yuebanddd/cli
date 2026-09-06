CREATE TABLE service_identities (
 issuer text NOT NULL,
 subject text NOT NULL,
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 PRIMARY KEY(issuer,subject)
);
CREATE TABLE login_attempts (
 state_hash text PRIMARY KEY,
 flow_id text NOT NULL REFERENCES oauth_flows(id) ON DELETE CASCADE,
 verifier bytea NOT NULL,
 nonce_hash text NOT NULL,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);
ALTER TABLE xyq_accounts ADD COLUMN binding_source text NOT NULL DEFAULT 'legacy_callback';
-- Legacy upstream identities are never promoted to service login identities.
DELETE FROM browser_sessions;
UPDATE oauth_flows SET consumed_at=now() WHERE consumed_at IS NULL;
UPDATE oauth_codes SET consumed_at=now() WHERE consumed_at IS NULL;
UPDATE oauth_families SET revoked_at=now() WHERE revoked_at IS NULL;
UPDATE xyq_bindings SET consumed_at=now() WHERE consumed_at IS NULL;
