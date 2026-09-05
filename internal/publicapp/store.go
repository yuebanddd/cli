package publicapp

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/001_public.sql
var migration001 string

type Store struct {
	DB    *sql.DB
	vault *vault
}

func Open(ctx context.Context, dsn string, key []byte) (*Store, error) {
	v, e := newVault(key)
	if e != nil {
		return nil, e
	}
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, errors.New("PostgreSQL unavailable")
	}
	return &Store{db, v}, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Migrate(ctx context.Context) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(719024013)`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS public_schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); e != nil {
		return e
	}
	var checksum string
	e = tx.QueryRowContext(ctx, `SELECT checksum FROM public_schema_migrations WHERE version=1`).Scan(&checksum)
	if errors.Is(e, sql.ErrNoRows) {
		if _, e = tx.ExecContext(ctx, migration001); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO public_schema_migrations(version,checksum) VALUES(1,$1)`, digest(migration001))
	} else if e == nil && checksum != digest(migration001) {
		return errors.New("migration checksum mismatch")
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Ready(ctx context.Context) error {
	var c string
	if e := s.DB.QueryRowContext(ctx, `SELECT checksum FROM public_schema_migrations WHERE version=1`).Scan(&c); e != nil {
		return e
	}
	if c != digest(migration001) {
		return errors.New("migration required")
	}
	return nil
}
func (s *Store) Audit(ctx context.Context, user, event, outcome, id string) error {
	_, e := s.DB.ExecContext(ctx, `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES(NULLIF($1,'')::uuid,$2,$3,$4)`, user, event, outcome, id)
	return e
}
func (s *Store) Allow(ctx context.Context, bucket string, limit int, window time.Duration) (bool, error) {
	var count int
	e := s.DB.QueryRowContext(ctx, `INSERT INTO rate_buckets(bucket,window_start,count) VALUES($1,now(),1) ON CONFLICT(bucket) DO UPDATE SET window_start=CASE WHEN rate_buckets.window_start < now()-$2::interval THEN now() ELSE rate_buckets.window_start END, count=CASE WHEN rate_buckets.window_start < now()-$2::interval THEN 1 ELSE rate_buckets.count+1 END RETURNING count`, bucket, fmt.Sprintf("%f seconds", window.Seconds())).Scan(&count)
	return count <= limit, e
}

type storedCredential struct {
	Credential auth.Credential `json:"credential"`
	AccessKey  string          `json:"access_key"`
}

func (s *Store) credential(ctx context.Context, user string) (*auth.Credential, error) {
	c, _, e := s.resolveCredential(ctx, user)
	return c, e
}
func (s *Store) resolveCredential(ctx context.Context, user string) (*auth.Credential, string, error) {
	var b, wrapped []byte
	var account, version string
	var expiry time.Time
	if e := s.DB.QueryRowContext(ctx, `SELECT a.id,c.ciphertext,c.encrypted_data_key,c.key_version,c.expires_at FROM xyq_accounts a JOIN xyq_credentials c ON c.account_id=a.id WHERE a.user_id=$1 AND a.status='active'`, user).Scan(&account, &b, &wrapped, &version, &expiry); e != nil {
		return nil, "", errors.New("reauthorization_required")
	}
	if !expiry.After(time.Now()) {
		return nil, "", errors.New("reauthorization_required")
	}
	if version != "local-v1" {
		return nil, "", errors.New("credential_unavailable")
	}
	key, e := s.vault.open(account, wrapped)
	if e != nil {
		return nil, "", errors.New("credential_unavailable")
	}
	defer clear(key)
	dataVault, e := newVault(key)
	if e != nil {
		return nil, "", errors.New("credential_unavailable")
	}
	plain, e := dataVault.open(account, b)
	if e != nil {
		return nil, "", errors.New("credential_unavailable")
	}
	defer clear(plain)
	var c storedCredential
	if json.Unmarshal(plain, &c) != nil || c.AccessKey == "" {
		return nil, "", errors.New("credential_unavailable")
	}
	c.Credential.AccessKey = c.AccessKey
	return &c.Credential, account, nil
}
func (s *Store) saveCredential(ctx context.Context, tx *sql.Tx, user string, c *auth.Credential) error {
	account := uuid()
	e := tx.QueryRowContext(ctx, `INSERT INTO xyq_accounts(id,user_id,xyq_uid,device_id,token_id,credential_scope,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(user_id,xyq_uid) DO UPDATE SET device_id=EXCLUDED.device_id,token_id=EXCLUDED.token_id,credential_scope=EXCLUDED.credential_scope,status='active',authorized_at=now(),expires_at=EXCLUDED.expires_at,updated_at=now() RETURNING id`, account, user, c.UID, c.DeviceID, c.TokenID, c.CredentialScope, time.Unix(c.ExpiredAt, 0)).Scan(&account)
	if e != nil {
		return e
	}
	b, e := json.Marshal(storedCredential{*c, c.AccessKey})
	if e != nil {
		return e
	}
	defer clear(b)
	key := make([]byte, 32)
	if _, e = rand.Read(key); e != nil {
		return e
	}
	defer clear(key)
	dataVault, e := newVault(key)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO xyq_credentials(account_id,ciphertext,encrypted_data_key,key_version,expires_at) VALUES($1,$2,$3,'local-v1',$4) ON CONFLICT(account_id) DO UPDATE SET ciphertext=EXCLUDED.ciphertext,encrypted_data_key=EXCLUDED.encrypted_data_key,key_version=EXCLUDED.key_version,expires_at=EXCLUDED.expires_at,updated_at=now()`, account, dataVault.seal(account, b), s.vault.seal(account, key), time.Unix(c.ExpiredAt, 0))
	return e
}
func (s *Store) Cleanup(ctx context.Context) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM work_leases WHERE expires_at<now()`,
		`DELETE FROM browser_sessions WHERE expires_at<now()`,
		`DELETE FROM oauth_flows WHERE expires_at<now()`,
		`DELETE FROM oauth_codes WHERE expires_at<now()-interval '1 day'`,
		// Keep consumed refresh tokens until the family expires for replay detection.
		`DELETE FROM oauth_families WHERE expires_at<now()`,
		`DELETE FROM oauth_tokens WHERE kind='access' AND expires_at<now()`,
		`DELETE FROM rate_buckets WHERE window_start<now()-interval '1 day'`,
		`UPDATE jobs SET state='uncertain',updated_at=now() WHERE state='pending' AND updated_at<now()-interval '20 minutes'`,
		// Retain the idempotency tombstone; drop URL-bearing responses after 30 days.
		`UPDATE jobs SET response=NULL,result_metadata=NULL WHERE (response IS NOT NULL OR result_metadata IS NOT NULL) AND updated_at<now()-interval '30 days'`,
		`DELETE FROM audit_events WHERE created_at<now()-interval '90 days'`,
	} {
		if _, e = tx.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	return tx.Commit()
}

//go:embed migrations/001_public.down.sql
var migration001Down string

func (s *Store) MigrateDown(ctx context.Context) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(719024013)`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, migration001Down); e != nil {
		return e
	}
	return tx.Commit()
}
