package publicapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/config"
)

type requestPolicy struct {
	app       *App
	principal principal
	accountID string
	requestID string
}

// ResolveAccessKey is bound to both the OAuth user and active account. The
// authorizer never reads env/keyring and fails for every HTTP method, including GET.
type userAuth struct {
	store         *Store
	user, account string
}

func (u *userAuth) ResolveAccessKey(ctx context.Context) (string, error) {
	c, account, e := u.store.resolveCredential(ctx, u.user)
	if e != nil {
		return "", e
	}
	if u.account != "" && account != u.account {
		return "", errors.New("reauthorization_required")
	}
	return c.AccessKey, nil
}
func (u *userAuth) Inject(ctx context.Context, r *http.Request) error {
	k, e := u.ResolveAccessKey(ctx)
	if e != nil {
		return e
	}
	r.Header.Set("Authorization", "Bearer "+k)
	return nil
}
func (u *userAuth) CredentialScope(ctx context.Context) (string, error) {
	_, e := u.ResolveAccessKey(ctx)
	return "public:" + u.user + ":" + u.account, e
}
func (u *userAuth) Status(ctx context.Context) (*auth.Status, error) {
	_, e := u.ResolveAccessKey(ctx)
	return &auth.Status{LoggedIn: e == nil, Source: "public"}, nil
}
func (u *userAuth) Login(context.Context, auth.LoginOptions) (*auth.Credential, error) {
	return nil, errors.New("reauthorization_required")
}
func (u *userAuth) Logout(context.Context, bool) error { return errors.New("use account disconnect") }
func (p *requestPolicy) runner() *common.Runner {
	a := &userAuth{p.app.store, p.principal.UserID, p.accountID}
	return &common.Runner{Config: &config.Config{BaseURL: config.DefaultBaseURL, HTTPTimeout: 2 * time.Minute}, Auth: a, Client: p.app.clientFactory(a)}
}

type resourceRef struct{ kind, id, parent string }

func resourceRefs(v any) []resourceRef {
	var refs []resourceRef
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			thread, _ := x["thread_id"].(string)
			for key, value := range x {
				normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
				kind := ""
				switch normalized {
				case "threadid":
					kind = "thread"
				case "runid":
					kind = "run"
				case "assetid", "assetids", "pippitassetid", "pippitassetids", "canvasid", "canvasassetid", "rootpippitassetid", "overviewpippitassetid", "allocatedassetids", "requestedassetids":
					kind = "asset"
				case "projectid":
					kind = "project"
				}
				if kind != "" {
					add := func(s string) {
						if s = strings.TrimSpace(s); s != "" {
							parent := ""
							if kind == "run" {
								parent = strings.TrimSpace(thread)
							}
							refs = append(refs, resourceRef{kind, s, parent})
						}
					}
					switch values := value.(type) {
					case string:
						add(values)
					case []any:
						for _, v := range values {
							if s, ok := v.(string); ok {
								add(s)
							}
						}
					}
				}
				walk(value)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(v)
	return refs
}
func (p *requestPolicy) owns(ctx context.Context, ref resourceRef) error {
	if ref.kind == "run" && ref.parent == "" {
		return errors.New("thread_id_required_for_run")
	}
	var one int
	e := p.app.store.DB.QueryRowContext(ctx, `SELECT 1 FROM tenant_resources WHERE user_id=$1 AND account_id=$2 AND kind=$3 AND resource_id=$4 AND ($3<>'run' OR parent_id=$5)`, p.principal.UserID, p.accountID, ref.kind, ref.id, ref.parent).Scan(&one)
	if e != nil {
		return errors.New("resource_not_found")
	}
	return nil
}
func (p *requestPolicy) checkInput(ctx context.Context, args map[string]any) error {
	for _, ref := range resourceRefs(args) {
		if e := p.owns(ctx, ref); e != nil {
			return e
		}
	}
	count := 0
	for _, key := range []string{"files", "images", "videos", "audios"} {
		if files, ok := args[key].([]any); ok {
			count += len(files)
		}
	}
	if count > p.app.cfg.MaxFiles {
		return errors.New("too_many_files")
	}
	return nil
}
func (p *requestPolicy) remember(ctx context.Context, tx *sql.Tx, refs []resourceRef) error {
	for _, ref := range refs {
		if ref.kind == "run" && ref.parent == "" {
			continue
		}
		r, e := tx.ExecContext(ctx, `INSERT INTO tenant_resources(user_id,account_id,kind,resource_id,parent_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(kind,resource_id) DO UPDATE SET resource_id=EXCLUDED.resource_id WHERE tenant_resources.user_id=EXCLUDED.user_id AND tenant_resources.account_id=EXCLUDED.account_id AND tenant_resources.parent_id=EXCLUDED.parent_id`, p.principal.UserID, p.accountID, ref.kind, ref.id, ref.parent)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return errors.New("upstream_resource_conflict")
		}
	}
	return nil
}
func (p *requestPolicy) Execute(ctx context.Context, tool string, readOnly bool, raw []byte, call func(context.Context) ([]byte, error)) ([]byte, error) {
	start := time.Now()
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return nil, errors.New("invalid_arguments")
	}
	// Revalidate the family for every call, including stateless MCP batch traffic.
	var one int
	if e := p.app.store.DB.QueryRowContext(ctx, `SELECT 1 FROM oauth_families WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL AND expires_at>now()`, p.principal.FamilyID, p.principal.UserID).Scan(&one); e != nil {
		return nil, errors.New("reauthorization_required")
	}
	if tool != "pippit_account_status" {
		_, account, e := p.app.store.resolveCredential(ctx, p.principal.UserID)
		if e != nil || account != p.accountID {
			return nil, errors.New("reauthorization_required")
		}
	}
	if e := p.checkInput(ctx, args); e != nil {
		_ = p.app.store.Audit(ctx, p.principal.UserID, tool, "ownership_denied", p.requestID)
		return nil, e
	}
	limit := 60
	if !readOnly {
		limit = 6
	}
	for _, b := range []struct {
		key   string
		limit int
	}{{"tool:" + p.principal.UserID + ":" + tool, limit}, {"user:" + p.principal.UserID, 120}} {
		ok, e := p.app.store.Allow(ctx, b.key, b.limit, time.Minute)
		if e != nil {
			return nil, errors.New("temporarily_unavailable")
		}
		if !ok {
			return nil, errors.New("rate_limit_exceeded")
		}
	}
	if e := p.app.store.Audit(ctx, p.principal.UserID, tool, "started", p.requestID); e != nil {
		return nil, errors.New("audit_unavailable")
	}
	key, _ := args["idempotency_key"].(string)
	hash := digest(tool + ":" + string(raw))
	jobID := ""
	if !readOnly {
		if len(key) < 8 || len(key) > 128 || strings.TrimSpace(key) != key {
			return nil, errors.New("idempotency_key_required_8_to_128_characters")
		}
		id := uuid()
		r, e := p.app.store.DB.ExecContext(ctx, `INSERT INTO jobs(id,user_id,account_id,idempotency_key,tool,request_hash,state) VALUES($1,$2,$3,$4,$5,$6,'pending') ON CONFLICT(user_id,idempotency_key) DO NOTHING`, id, p.principal.UserID, p.accountID, key, tool, hash)
		if e != nil {
			return nil, errors.New("temporarily_unavailable")
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			var priorHash, priorTool, state string
			var response []byte
			e = p.app.store.DB.QueryRowContext(ctx, `SELECT request_hash,tool,state,response FROM jobs WHERE user_id=$1 AND account_id=$2 AND idempotency_key=$3`, p.principal.UserID, p.accountID, key).Scan(&priorHash, &priorTool, &state, &response)
			if e != nil {
				return nil, errors.New("idempotency_conflict")
			}
			if priorHash != hash || priorTool != tool {
				return nil, errors.New("idempotency_conflict")
			}
			if state == "completed" && len(response) > 0 {
				return response, nil
			}
			return nil, errors.New("operation_pending_or_uncertain: query existing job; do not generate again")
		}
		jobID = id
	}
	// All work leases are durable and shared by replicas. They expire after the
	// hard request deadline and are released on normal completion.
	release, e := p.acquire(ctx, tool, readOnly)
	if e != nil {
		if jobID != "" {
			_, _ = p.app.store.DB.ExecContext(ctx, `DELETE FROM jobs WHERE id=$1 AND state='pending'`, jobID)
		}
		return nil, e
	}
	defer release()
	output, callErr := call(ctx)
	doneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if callErr != nil {
		if jobID != "" {
			_, _ = p.app.store.DB.ExecContext(doneCtx, `UPDATE jobs SET state='uncertain',updated_at=now() WHERE id=$1`, jobID)
		}
		_ = p.audit(doneCtx, tool, "failed", time.Since(start), "upstream_error", "", "")
		return nil, errors.New("upstream_error: outcome may be uncertain; query status before retrying")
	}
	if len(output) > 2<<20 {
		return nil, errors.New("result_metadata_too_large")
	}
	var data map[string]any
	if json.Unmarshal(output, &data) != nil {
		return nil, errors.New("invalid_upstream_metadata")
	}
	thread, _ := data["thread_id"].(string)
	run, _ := data["run_id"].(string)
	if expected, _ := args["thread_id"].(string); expected != "" && thread != "" && strings.TrimSpace(expected) != thread {
		return nil, errors.New("upstream_thread_mismatch")
	}
	if expected, _ := args["run_id"].(string); expected != "" && run != "" && strings.TrimSpace(expected) != run {
		return nil, errors.New("upstream_run_mismatch")
	}
	if jobID != "" && run != "" {
		data["status"] = "submitted"
		output, _ = json.Marshal(data)
	}
	tx, e := p.app.store.DB.BeginTx(doneCtx, nil)
	if e != nil {
		return nil, errors.New("result_persistence_failed: do not repeat generation")
	}
	defer tx.Rollback()
	// Only successful CREATE/UPLOAD results may introduce new ownership. Reads
	// may reveal assets of an already-owned thread but never claim foreign IDs.
	refs := resourceRefs(data)
	if e = p.remember(doneCtx, tx, refs); e != nil {
		return nil, errors.New("result_ownership_conflict")
	}
	if jobID != "" {
		if _, e = tx.ExecContext(doneCtx, `UPDATE jobs SET response=$1,state='completed',thread_id=NULLIF($2,''),run_id=NULLIF($3,''),updated_at=now() WHERE id=$4`, output, thread, run, jobID); e != nil {
			return nil, errors.New("result_persistence_failed: do not repeat generation")
		}
		for _, ref := range refs {
			if ref.kind == "asset" {
				if _, e = tx.ExecContext(doneCtx, `INSERT INTO job_assets(job_id,asset_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, jobID, ref.id); e != nil {
					return nil, errors.New("result_persistence_failed")
				}
			}
		}
	}
	if completed, ok := data["completed"].(bool); ok && completed {
		if _, e = tx.ExecContext(doneCtx, `UPDATE jobs SET generation_finished=true,result_metadata=$5,updated_at=now() WHERE user_id=$1 AND account_id=$2 AND thread_id=$3 AND run_id=$4`, p.principal.UserID, p.accountID, thread, run, output); e != nil {
			return nil, errors.New("result_persistence_failed")
		}
	}
	if _, e = tx.ExecContext(doneCtx, `INSERT INTO audit_events(user_id,event,outcome,correlation_id,thread_id,run_id,latency_ms) VALUES($1,$2,'success',$3,NULLIF($4,''),NULLIF($5,''),$6)`, p.principal.UserID, tool, p.requestID, thread, run, time.Since(start).Milliseconds()); e != nil {
		return nil, errors.New("audit_unavailable")
	}
	if e = tx.Commit(); e != nil {
		return nil, errors.New("result_persistence_failed: do not repeat generation")
	}
	p.app.logger.Info("tool completed", "request_id", p.requestID, "user_id", p.principal.UserID, "tool", tool, "duration_ms", time.Since(start).Milliseconds(), "upstream_status", "success")
	return output, nil
}
func (p *requestPolicy) audit(ctx context.Context, tool, outcome string, d time.Duration, class, thread, run string) error {
	_, e := p.app.store.DB.ExecContext(ctx, `INSERT INTO audit_events(user_id,event,outcome,correlation_id,latency_ms,error_class,thread_id,run_id) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''))`, p.principal.UserID, tool, outcome, p.requestID, d.Milliseconds(), class, thread, run)
	return e
}
func (p *requestPolicy) acquire(ctx context.Context, tool string, readOnly bool) (func(), error) {
	tx, e := p.app.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(719024014)`); e != nil {
		return nil, e
	}
	kind := "read"
	limit := 20
	if !readOnly {
		kind = "write"
		limit = 1
	}
	var userCount, globalCount int
	e = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE user_id=$1),count(*) FROM work_leases WHERE expires_at>now() AND kind=$2`, p.principal.UserID, kind).Scan(&userCount, &globalCount)
	if e != nil {
		return nil, e
	}
	if userCount >= limit || globalCount >= p.app.cfg.GlobalConcurrent {
		return nil, errors.New("concurrency_limit_exceeded")
	}
	// Count submitted runs still active upstream, not only HTTP submissions.
	if !readOnly && isGeneration(tool) {
		var active int
		e = tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE user_id=$1 AND run_id IS NOT NULL AND generation_finished=false`, p.principal.UserID).Scan(&active)
		if e != nil {
			return nil, e
		}
		if active >= p.app.cfg.UserActiveJobs {
			return nil, errors.New("active_job_limit_exceeded: query existing jobs")
		}
	}
	id := randomToken()
	_, e = tx.ExecContext(ctx, `INSERT INTO work_leases(id,user_id,kind,expires_at) VALUES($1,$2,$3,now()+interval '16 minutes')`, id, p.principal.UserID, kind)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.app.store.DB.ExecContext(ctx, `DELETE FROM work_leases WHERE id=$1`, id)
	}, nil
}
func isGeneration(tool string) bool {
	return strings.Contains(tool, "generate_") || strings.Contains(tool, "submit") || strings.Contains(tool, "super_resolution") || strings.Contains(tool, "erase_video")
}

var _ common.AuthManager = (*userAuth)(nil)
