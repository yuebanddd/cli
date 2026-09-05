package publicapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
	"github.com/Pippit-dev/pippit-cli/internal/config"
)

const browserCookie = "__Host-pippit-browser"
const sessionCookie = "__Host-pippit-session"

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>授权 ChatGPT</title><body><main><h1>授权 ChatGPT 使用小云雀</h1><p>授权客户端：{{.Client}}</p><p>允许上传你提供的素材、创建和修改作品、读取你的任务结果。生成或编辑会消耗你的小云雀积分。产物由小云雀托管。</p><p>{{.Status}}</p><form method="post" action="/bind/start" target="_blank"><input type="hidden" name="flow" value="{{.Flow}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>前往小云雀官网登录并授权</button></form><p>完成小云雀授权后，回到本页继续。</p><form method="post" action="/oauth/consent"><input type="hidden" name="flow" value="{{.Flow}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button name="decision" value="allow">允许并返回 ChatGPT</button><button name="decision" value="deny">取消</button></form><form method="post" action="/account/unlink"><input type="hidden" name="flow" value="{{.Flow}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>解除小云雀绑定并撤销 ChatGPT 授权</button></form><form method="post" action="/account/logout"><input type="hidden" name="flow" value="{{.Flow}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>退出本服务账号</button></form></main></body></html>`))

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
	_ = a.store.DB.QueryRowContext(r.Context(), `SELECT user_id FROM browser_sessions WHERE token_hash=$1 AND expires_at>now()`, digest(t)).Scan(&user)
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
	data := map[string]string{"Flow": f.ID, "CSRF": csrf(r, f.ID), "Client": f.Data.ClientID, "Status": "请先连接你的小云雀账号。"}
	if f.UserID != "" {
		if c, e := a.store.credential(r.Context(), f.UserID); e == nil {
			data["Status"] = "已连接小云雀；凭据有效期至 " + time.Unix(c.ExpiredAt, 0).UTC().Format(time.RFC3339)
		}
	}
	_ = consentPage.Execute(w, data)
}
func (a *App) bindStart(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, false)
	if !ok {
		return
	}
	device, e := auth.NewRemoteDeviceID()
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	secret, e := auth.NewRemoteBindingSecret()
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	var token, account string
	if old, e := a.store.credential(r.Context(), f.UserID); e == nil {
		device = old.DeviceID
		token = old.TokenID
		account = auth.AccountBindingForUID(old.UID)
	}
	id := randomToken()
	callback := a.cfg.Issuer + "/bind/callback?binding=" + id
	loginURL, e := auth.BuildRemoteLoginURL(callback, device, token, account, secret, false)
	if e != nil {
		http.Error(w, "unable to start binding", 503)
		return
	}
	_, e = a.store.DB.ExecContext(r.Context(), `INSERT INTO xyq_bindings(id,user_id,flow_id,secret_hash,device_id,expires_at) VALUES($1,NULLIF($2,'')::uuid,$3,$4,$5,now()+interval '5 minutes')`, id, f.UserID, f.ID, digest(secret), device)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	http.Redirect(w, r, loginURL, 303)
}
func (a *App) bindCallback(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != config.DefaultBaseURL {
		http.Error(w, "invalid origin", 403)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", config.DefaultBaseURL)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var p auth.RemoteAccessKeyPayload
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || json.NewDecoder(r.Body).Decode(&p) != nil {
		http.Error(w, "invalid callback", 400)
		return
	}
	id := r.URL.Query().Get("binding")
	callback := a.cfg.Issuer + "/bind/callback?binding=" + id
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	var expectedUser sql.NullString
	var device, secretHash, flowID string
	e = tx.QueryRowContext(r.Context(), `SELECT b.user_id,b.device_id,b.secret_hash,b.flow_id FROM xyq_bindings b JOIN oauth_flows f ON f.id=b.flow_id WHERE b.id=$1 AND b.expires_at>now() AND b.consumed_at IS NULL AND f.expires_at>now() AND f.consumed_at IS NULL FOR UPDATE OF b,f`, id).Scan(&expectedUser, &device, &secretHash, &flowID)
	if e != nil || !equal(secretHash, digest(p.RandomSecretKey)) || auth.ValidateRemoteAccessKeyPayload(p, callback, p.RandomSecretKey) != nil || p.ExpiredAt <= time.Now().Unix() {
		http.Error(w, "invalid or expired binding", 400)
		return
	}
	// Binding and Origin prevent flow confusion, but cannot prove a UID. Only a
	// verified upstream subject may select an existing app user.
	subject, e := a.authorization.Verify(r.Context(), p)
	if e != nil || subject != p.UID {
		http.Error(w, "upstream identity could not be verified", 403)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,719))`, subject); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	var user string
	e = tx.QueryRowContext(r.Context(), `SELECT user_id FROM upstream_identities WHERE subject=$1`, subject).Scan(&user)
	if errors.Is(e, sql.ErrNoRows) {
		if expectedUser.Valid {
			http.Error(w, "account mismatch; disconnect first", 409)
			return
		}
		user = uuid()
		if _, e = tx.ExecContext(r.Context(), `INSERT INTO app_users(id) VALUES($1)`, user); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		if _, e = tx.ExecContext(r.Context(), `INSERT INTO upstream_identities(subject,user_id) VALUES($1,$2)`, subject, user); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
	} else if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if expectedUser.Valid && expectedUser.String != user {
		http.Error(w, "account mismatch; disconnect first", 409)
		return
	}
	var locked string
	if e = tx.QueryRowContext(r.Context(), `SELECT id FROM app_users WHERE id=$1 AND status='active' FOR UPDATE`, user).Scan(&locked); e != nil {
		http.Error(w, "account unavailable", 403)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `UPDATE oauth_flows SET user_id=$1 WHERE id=$2`, user, flowID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	c := auth.CredentialFromRemotePayload(device, p)
	if a.store.saveCredential(r.Context(), tx, user, c) != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `UPDATE xyq_bindings SET consumed_at=now() WHERE flow_id=$1`, flowID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'xyq.bind','success',$2)`, user, id); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if tx.Commit() != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "授权完成，请返回原页面继续。"})
}
func (a *App) consent(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, true)
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
			http.Error(w, "complete Xiaoyunque authorization first", 409)
			return
		}
	}
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(r.Context(), `UPDATE oauth_flows SET consumed_at=now() WHERE id=$1 AND consumed_at IS NULL AND expires_at>now() AND user_id=$2`, f.ID, f.UserID)
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
		sessionToken := randomToken()
		if _, e = tx.ExecContext(r.Context(), `INSERT INTO browser_sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '8 hours')`, digest(sessionToken), f.UserID); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		setCookie(w, sessionCookie, sessionToken, 8*3600)
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
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'oauth.consent',$2,$3)`, f.UserID, decision, f.ID); e != nil {
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
	if _, e := a.store.DB.ExecContext(r.Context(), `DELETE FROM browser_sessions WHERE token_hash=$1 AND user_id=$2`, digest(cookieValue(r, sessionCookie)), f.UserID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	setCookie(w, sessionCookie, "", -1)
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
