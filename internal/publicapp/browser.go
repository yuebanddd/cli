package publicapp

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"
)

const browserCookie = "__Host-pippit-browser"
const sessionCookie = "__Host-pippit-session"

//go:embed consent.html
var consentHTML string

//go:embed consent.css
var consentCSS string

var consentPage = template.Must(template.New("consent").Parse(consentHTML))

type flow struct {
	ID, UserID, BrowserHash string
	Data                    authorization
}

func cookieValue(r *http.Request, name string) string {
	c, e := r.Cookie(name)
	if e != nil || len(c.Value) != 43 {
		return ""
	}
	return c.Value
}
func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}
func (a *App) session(r *http.Request) string {
	var user string
	t := cookieValue(r, sessionCookie)
	if t == "" {
		return ""
	}
	_ = a.store.DB.QueryRowContext(r.Context(), `SELECT s.user_id FROM browser_sessions s JOIN app_users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND u.status='active'`, digest(t)).Scan(&user)
	return user
}
func csrf(r *http.Request, id string) string {
	return digest("csrf:" + cookieValue(r, browserCookie) + ":" + id)
}
func (a *App) loadFlow(r *http.Request, id string) (flow, error) {
	f := flow{ID: id}
	var b []byte
	var user sql.NullString
	e := a.store.DB.QueryRowContext(r.Context(), `SELECT user_id,browser_hash,data FROM oauth_flows WHERE id=$1 AND expires_at>now() AND consumed_at IS NULL`, id).Scan(&user, &f.BrowserHash, &b)
	if e != nil || cookieValue(r, browserCookie) == "" || !equal(f.BrowserHash, digest(cookieValue(r, browserCookie))) {
		return f, errors.New("invalid flow")
	}
	f.UserID = user.String
	if f.UserID != "" && f.UserID != a.session(r) {
		return f, errors.New("session expired or account mismatch")
	}
	e = json.Unmarshal(b, &f.Data)
	return f, e
}
func (a *App) formFlow(w http.ResponseWriter, r *http.Request, requireUser bool) (flow, bool) {
	if r.Header.Get("Origin") != a.cfg.Issuer || !parseForm(w, r) {
		http.Error(w, "invalid form origin", 403)
		return flow{}, false
	}
	f, e := a.loadFlow(r, r.PostForm.Get("flow"))
	if e != nil || !equal(r.PostForm.Get("csrf"), csrf(r, f.ID)) {
		http.Error(w, "invalid or expired flow", 403)
		return f, false
	}
	if requireUser && f.UserID == "" {
		http.Error(w, "sign in again", 401)
		return f, false
	}
	return f, true
}
func (a *App) authorize(w http.ResponseWriter, r *http.Request) {
	if id := r.URL.Query().Get("flow"); id != "" {
		f, e := a.loadFlow(r, id)
		if e != nil {
			http.Error(w, "invalid flow", 400)
			return
		}
		a.renderFlow(w, r, f)
		return
	}
	authz, e := a.validAuthorization(r.Context(), r.URL.Query())
	if e != nil {
		oauthError(w, e.Error())
		return
	}
	browser := cookieValue(r, browserCookie)
	if browser == "" {
		browser = randomToken()
		setCookie(w, browserCookie, browser, 86400)
		r.AddCookie(&http.Cookie{Name: browserCookie, Value: browser})
	}
	f := flow{ID: randomToken(), UserID: a.session(r), BrowserHash: digest(browser), Data: authz}
	b, _ := json.Marshal(authz)
	_, e = a.store.DB.ExecContext(r.Context(), `INSERT INTO oauth_flows(id,browser_hash,user_id,data,expires_at) VALUES($1,$2,NULLIF($3,'')::uuid,$4,now()+interval '10 minutes')`, f.ID, f.BrowserHash, f.UserID, b)
	if e != nil {
		http.Error(w, "temporarily unavailable", 503)
		return
	}
	a.renderFlow(w, r, f)
}
func (a *App) renderFlow(w http.ResponseWriter, r *http.Request, f flow) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]string{"Flow": f.ID, "CSRF": csrf(r, f.ID), "Client": f.Data.ClientID, "User": f.UserID, "Status": "尚未绑定 Access Key。"}
	if f.UserID != "" {
		if c, e := a.store.credential(r.Context(), f.UserID); e == nil {
			data["Bound"] = "true"
			data["Status"] = "Access Key 已加密保存；本服务使用截止时间：" + time.Unix(c.ExpiredAt, 0).UTC().Format(time.RFC3339)
		}
	}
	_ = consentPage.Execute(w, data)
}
func (a *App) consent(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, false)
	if !ok {
		return
	}
	target, _ := url.Parse(f.Data.RedirectURI)
	q := target.Query()
	q.Set("state", f.Data.State)
	q.Set("iss", a.cfg.Issuer)
	decision := r.PostForm.Get("decision")
	if decision != "allow" && decision != "deny" {
		http.Error(w, "invalid decision", 400)
		return
	}
	if decision == "allow" {
		if _, e := a.store.credential(r.Context(), f.UserID); e != nil {
			http.Error(w, "sign in and bind an Access Key first", 409)
			return
		}
	}
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	if f.UserID != "" {
		var id string
		if e = tx.QueryRowContext(r.Context(), `SELECT id FROM app_users WHERE id=$1 AND status='active' FOR UPDATE`, f.UserID).Scan(&id); e != nil {
			http.Error(w, "account unavailable", 403)
			return
		}
	}
	result, e := tx.ExecContext(r.Context(), `UPDATE oauth_flows SET consumed_at=now() WHERE id=$1 AND consumed_at IS NULL AND expires_at>now() AND user_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid`, f.ID, f.UserID)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		http.Error(w, "flow expired or already used", 409)
		return
	}
	if decision == "allow" {
		code := randomToken()
		_, e = tx.ExecContext(r.Context(), `INSERT INTO oauth_codes(code_hash,user_id,client_id,redirect_uri,challenge,resource,scope,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '60 seconds')`, digest(code), f.UserID, f.Data.ClientID, f.Data.RedirectURI, f.Data.Challenge, f.Data.Resource, scope)
		q.Set("code", code)
	} else {
		q.Set("error", "access_denied")
	}
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES(NULLIF($1,'')::uuid,'oauth.consent',$2,$3)`, f.UserID, decision, f.ID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if tx.Commit() != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), 303)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, true)
	if !ok {
		return
	}
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(r.Context(), `DELETE FROM browser_sessions WHERE token_hash=$1 AND user_id=$2`, digest(cookieValue(r, sessionCookie)), f.UserID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	// A flow itself grants browser authority, including before a session exists.
	// Invalidate every tab in this browser so Back cannot authorize after logout.
	if _, e = tx.ExecContext(r.Context(), `UPDATE oauth_flows SET consumed_at=now() WHERE browser_hash=$1 AND consumed_at IS NULL`, f.BrowserHash); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'account.logout','success',$2)`, f.UserID, f.ID); e != nil || tx.Commit() != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	setCookie(w, sessionCookie, "", -1)
	setCookie(w, browserCookie, "", -1)
	writeJSON(w, 200, map[string]bool{"logged_out": true})
}
func (a *App) unlink(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, true)
	if !ok {
		return
	}
	if e := a.unlinkUser(r.Context(), f.UserID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	writeJSON(w, 200, map[string]bool{"unlinked": true})
}
func (a *App) unlinkUser(ctx context.Context, user string) error {
	tx, e := a.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var id string
	if e = tx.QueryRowContext(ctx, `SELECT id FROM app_users WHERE id=$1 FOR UPDATE`, user).Scan(&id); e != nil {
		return e
	}
	for _, q := range []string{`DELETE FROM xyq_credentials WHERE account_id IN (SELECT id FROM xyq_accounts WHERE user_id=$1)`, `UPDATE xyq_accounts SET status='disconnected' WHERE user_id=$1`, `DELETE FROM browser_sessions WHERE user_id=$1`, `UPDATE oauth_families SET revoked_at=now() WHERE user_id=$1`, `UPDATE xyq_bindings SET consumed_at=now() WHERE user_id=$1`, `UPDATE oauth_flows SET consumed_at=now() WHERE user_id=$1`, `UPDATE oauth_codes SET consumed_at=now() WHERE user_id=$1`} {
		if _, e = tx.ExecContext(ctx, q, user); e != nil {
			return e
		}
	}
	return tx.Commit()
}
