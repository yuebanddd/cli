-- Destructive: use only for a disposable environment or after an approved backup.
DROP TABLE IF EXISTS canvas_state,job_assets,work_leases,audit_events,rate_buckets,jobs,tenant_resources,oauth_tokens,oauth_families,oauth_codes,xyq_credentials,upstream_identities,xyq_accounts,xyq_bindings,oauth_flows,browser_sessions,oauth_clients,app_users CASCADE;
DELETE FROM public_schema_migrations WHERE version=1;
