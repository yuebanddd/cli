DROP TABLE IF EXISTS login_attempts,service_identities;
ALTER TABLE IF EXISTS xyq_accounts DROP COLUMN IF EXISTS binding_source;
DELETE FROM public_schema_migrations WHERE version=2;
