package publicapp

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
)

var bearerKey = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

func (a *App) bindKey(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, true)
	if !ok {
		return
	}
	key := r.PostForm.Get("access_key")
	ttl := map[string]time.Duration{"1": 24 * time.Hour, "7": 7 * 24 * time.Hour, "30": 30 * 24 * time.Hour}[r.PostForm.Get("days")]
	if len(key) == 0 || len(key) > 8192 || !bearerKey.MatchString(key) || ttl == 0 || r.PostForm.Get("confirm") != "bind" {
		http.Error(w, "invalid Access Key, retention period or confirmation", 400)
		return
	}
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	var locked string
	if e = tx.QueryRowContext(r.Context(), `SELECT id FROM app_users WHERE id=$1 AND status='active' FOR UPDATE`, f.UserID).Scan(&locked); e != nil {
		http.Error(w, "account unavailable", 403)
		return
	}
	result, e := tx.ExecContext(r.Context(), `UPDATE oauth_flows SET consumed_at=now() WHERE id=$1 AND user_id=$2 AND consumed_at IS NULL AND expires_at>now() AND EXISTS (SELECT 1 FROM browser_sessions WHERE token_hash=$3 AND user_id=$2 AND expires_at>now())`, f.ID, f.UserID, digest(cookieValue(r, sessionCookie)))
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		http.Error(w, "expired flow", 403)
		return
	}
	// A manually supplied key cannot attest upstream identity. Never reuse an old
	// account's resource ownership, even when the submitted key looks unchanged.
	for _, query := range []string{
		`DELETE FROM xyq_credentials WHERE account_id IN (SELECT id FROM xyq_accounts WHERE user_id=$1)`,
		`UPDATE xyq_accounts SET status='disconnected' WHERE user_id=$1`,
		`UPDATE oauth_families SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`,
		`UPDATE oauth_codes SET consumed_at=now() WHERE user_id=$1 AND consumed_at IS NULL`,
		`UPDATE oauth_flows SET consumed_at=now() WHERE user_id=$1 AND consumed_at IS NULL`,
	} {
		if _, e = tx.ExecContext(r.Context(), query, f.UserID); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
	}
	bindingID := "manual:" + uuid()
	c := &auth.Credential{Version: 1, UID: bindingID, CredentialScope: bindingID, AccessKey: key, ExpiredAt: time.Now().Add(ttl).Unix()}
	if a.store.saveCredential(r.Context(), tx, f.UserID, c) != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `UPDATE xyq_accounts SET binding_source='manual_unverified' WHERE user_id=$1 AND status='active'`, f.UserID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	newFlow := randomToken()
	data, _ := json.Marshal(f.Data)
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO oauth_flows(id,user_id,browser_hash,data,expires_at) VALUES($1,$2,$3,$4,now()+interval '10 minutes')`, newFlow, f.UserID, f.BrowserHash, data); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'xyq.bind.manual','stored_unverified',$2)`, f.UserID, f.ID); e != nil || tx.Commit() != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	http.Redirect(w, r, "/oauth/authorize?flow="+newFlow, 303)
}
