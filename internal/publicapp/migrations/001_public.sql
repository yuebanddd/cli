CREATE TABLE app_users (
 id uuid PRIMARY KEY,
 status text NOT NULL DEFAULT 'active',
 updated_at timestamptz NOT NULL DEFAULT now(),
 last_seen_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE oauth_clients (
 id text PRIMARY KEY,
 redirect_uris jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE browser_sessions (
 token_hash text PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 expires_at timestamptz NOT NULL
);
CREATE TABLE oauth_flows (
 id text PRIMARY KEY,
 browser_hash text NOT NULL,
 user_id uuid REFERENCES app_users(id) ON DELETE CASCADE,
 data jsonb NOT NULL,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);
CREATE TABLE xyq_bindings (
 id text PRIMARY KEY,
 user_id uuid REFERENCES app_users(id) ON DELETE CASCADE,
 flow_id text NOT NULL REFERENCES oauth_flows(id) ON DELETE CASCADE,
 secret_hash text NOT NULL,
 device_id text NOT NULL,
 expected_user_id uuid REFERENCES app_users(id) ON DELETE CASCADE,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);
CREATE TABLE xyq_accounts (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 xyq_uid text NOT NULL,
 device_id text NOT NULL,
 token_id text NOT NULL,
 credential_scope text NOT NULL,
 status text NOT NULL DEFAULT 'active',
 authorized_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,xyq_uid),
 UNIQUE(id,user_id)
);
CREATE UNIQUE INDEX xyq_one_active ON xyq_accounts(user_id) WHERE status='active';
-- Verified upstream subject is a login identity; account rows can later support
-- additional bindings without changing the token subject or job ownership.
CREATE TABLE upstream_identities (
 subject text PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE
);
CREATE TABLE xyq_credentials (
 account_id uuid PRIMARY KEY REFERENCES xyq_accounts(id) ON DELETE CASCADE,
 encrypted_data_key bytea NOT NULL,
 ciphertext bytea NOT NULL,
 key_version text NOT NULL,
 expires_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE oauth_codes (
 code_hash text PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 client_id text NOT NULL REFERENCES oauth_clients(id),
 redirect_uri text NOT NULL,
 challenge text NOT NULL,
 resource text NOT NULL,
 scope text NOT NULL,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);
CREATE TABLE oauth_families (
 id text PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 client_id text NOT NULL REFERENCES oauth_clients(id),
 resource text NOT NULL,
 scope text NOT NULL,
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz
);
CREATE TABLE oauth_tokens (
 token_hash text PRIMARY KEY,
 family_id text NOT NULL REFERENCES oauth_families(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK (kind IN ('access','refresh')),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);
CREATE INDEX oauth_tokens_family ON oauth_tokens(family_id);
CREATE TABLE tenant_resources (
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 account_id uuid NOT NULL,
 kind text NOT NULL CHECK (kind IN ('thread','run','asset','project')),
 resource_id text NOT NULL,
 parent_id text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,kind,resource_id),
 UNIQUE(kind,resource_id),
 FOREIGN KEY(account_id,user_id) REFERENCES xyq_accounts(id,user_id) ON DELETE CASCADE
);
CREATE TABLE jobs (
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 id uuid UNIQUE NOT NULL,
 account_id uuid NOT NULL,
 thread_id text,
 run_id text,
 generation_finished boolean NOT NULL DEFAULT false,
 result_metadata jsonb,
 idempotency_key text NOT NULL,
 tool text NOT NULL,
 request_hash text NOT NULL,
 state text NOT NULL CHECK (state IN ('pending','completed','uncertain')),
 response jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,idempotency_key),
 FOREIGN KEY(account_id,user_id) REFERENCES xyq_accounts(id,user_id) ON DELETE CASCADE
);
CREATE INDEX jobs_age ON jobs(updated_at);
CREATE TABLE rate_buckets (
 bucket text PRIMARY KEY,
 window_start timestamptz NOT NULL,
 count integer NOT NULL
);
CREATE TABLE audit_events (
 id bigserial PRIMARY KEY,
 user_id uuid,
 event text NOT NULL,
 outcome text NOT NULL,
 correlation_id text NOT NULL,
 thread_id text,
 run_id text,
 latency_ms bigint,
 error_class text,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_user_time ON audit_events(user_id,created_at);

CREATE TABLE work_leases (
 id text PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
 kind text NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX work_leases_active ON work_leases(kind,expires_at);
CREATE TABLE job_assets (
 job_id uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
 asset_id text NOT NULL,
 metadata jsonb NOT NULL DEFAULT '{}',
 PRIMARY KEY(job_id,asset_id)
);

CREATE TABLE canvas_state (
 user_id uuid NOT NULL,
 account_id uuid NOT NULL,
 canvas_id text NOT NULL,
 state jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,account_id,canvas_id),
 FOREIGN KEY(account_id,user_id) REFERENCES xyq_accounts(id,user_id) ON DELETE CASCADE
);
