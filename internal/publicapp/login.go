package publicapp

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type serviceLogin struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	origin   string
}

func validLoginURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func newServiceLogin(ctx context.Context, c Config) (*serviceLogin, error) {
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("login redirects forbidden") }}
	if custom, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		client.Transport = custom.Transport
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, e := oidc.NewProvider(ctx, c.LoginIssuer)
	if e != nil {
		return nil, errors.New("OIDC discovery unavailable")
	}
	endpoint := provider.Endpoint()
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if provider.Claims(&metadata) != nil || !validLoginURL(endpoint.AuthURL) || !validLoginURL(endpoint.TokenURL) || !validLoginURL(metadata.JWKS) {
		return nil, errors.New("OIDC endpoints must use HTTPS without query or userinfo")
	}
	u, _ := url.Parse(endpoint.AuthURL)
	return &serviceLogin{
		oauth:    oauth2.Config{ClientID: c.LoginClientID, ClientSecret: c.LoginClientSecret, RedirectURL: c.Issuer + "/account/callback", Endpoint: endpoint, Scopes: []string{oidc.ScopeOpenID}},
		verifier: provider.VerifierContext(oidc.ClientContext(context.Background(), client), &oidc.Config{ClientID: c.LoginClientID}),
		client:   client, origin: u.Scheme + "://" + u.Host,
	}, nil
}

func (a *App) loginStart(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, false)
	if !ok {
		return
	}
	if f.UserID != "" || a.session(r) != "" {
		http.Error(w, "already signed in; sign out before switching accounts", 409)
		return
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	// Lock the flow so concurrent login starts leave only one usable attempt.
	var id string
	if e = tx.QueryRowContext(r.Context(), `SELECT id FROM oauth_flows WHERE id=$1 AND consumed_at IS NULL AND expires_at>now() AND user_id IS NULL FOR UPDATE`, f.ID).Scan(&id); e != nil {
		http.Error(w, "expired flow", 403)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `UPDATE login_attempts SET consumed_at=now() WHERE flow_id=$1 AND consumed_at IS NULL`, f.ID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO login_attempts(state_hash,flow_id,verifier,nonce_hash,expires_at) VALUES($1,$2,$3,$4,now()+interval '5 minutes')`, digest(state), f.ID, a.store.vault.seal(digest(state), []byte(verifier)), digest(nonce)); e != nil || tx.Commit() != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	http.Redirect(w, r, a.login.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusSeeOther)
}

func (a *App) loginCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if len(state) != 43 || len(q["state"]) != 1 || len(q["code"]) > 1 || len(q.Get("code")) > 8192 || cookieValue(r, browserCookie) == "" {
		http.Error(w, "invalid login response", 400)
		return
	}
	var flowID, nonceHash string
	var encrypted []byte
	// Consume before token exchange; failed exchanges require a fresh login.
	e := a.store.DB.QueryRowContext(r.Context(), `UPDATE login_attempts l SET consumed_at=now() FROM oauth_flows f WHERE l.state_hash=$1 AND l.flow_id=f.id AND l.expires_at>now() AND l.consumed_at IS NULL AND f.browser_hash=$2 AND f.expires_at>now() AND f.consumed_at IS NULL AND f.user_id IS NULL RETURNING l.flow_id,l.verifier,l.nonce_hash`, digest(state), digest(cookieValue(r, browserCookie))).Scan(&flowID, &encrypted, &nonceHash)
	if e != nil {
		http.Error(w, "invalid or expired login", 403)
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		http.Redirect(w, r, "/oauth/authorize?flow="+flowID, 303)
		return
	}
	verifier, e := a.store.vault.open(digest(state), encrypted)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer clear(verifier)
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), a.login.client), 20*time.Second)
	defer cancel()
	token, e := a.login.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(string(verifier)))
	if e != nil {
		http.Error(w, "login verification failed", 403)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	idToken, e := a.login.verifier.Verify(ctx, raw)
	if e != nil || idToken.Subject == "" || len(idToken.Subject) > 512 || !equal(digest(idToken.Nonce), nonceHash) {
		http.Error(w, "login verification failed", 403)
		return
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if idToken.Claims(&claims) != nil || (claims.AuthorizedParty != "" && claims.AuthorizedParty != a.cfg.LoginClientID) || (len(idToken.Audience) > 1 && claims.AuthorizedParty != a.cfg.LoginClientID) {
		http.Error(w, "login verification failed", 403)
		return
	}
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,719))`, digest(idToken.Issuer+"\x00"+idToken.Subject)); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	var user string
	e = tx.QueryRowContext(r.Context(), `SELECT user_id FROM service_identities WHERE issuer=$1 AND subject=$2`, idToken.Issuer, idToken.Subject).Scan(&user)
	if errors.Is(e, sql.ErrNoRows) {
		user = uuid()
		if _, e = tx.ExecContext(r.Context(), `INSERT INTO app_users(id) VALUES($1)`, user); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		if _, e = tx.ExecContext(r.Context(), `INSERT INTO service_identities(issuer,subject,user_id) VALUES($1,$2,$3)`, idToken.Issuer, idToken.Subject, user); e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
	} else if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	var locked string
	if e = tx.QueryRowContext(r.Context(), `SELECT id FROM app_users WHERE id=$1 AND status='active' FOR UPDATE`, user).Scan(&locked); e != nil {
		http.Error(w, "account unavailable", 403)
		return
	}
	browser, session := randomToken(), randomToken()
	result, e := tx.ExecContext(r.Context(), `UPDATE oauth_flows SET user_id=$1,browser_hash=$2 WHERE id=$3 AND user_id IS NULL AND browser_hash=$4 AND consumed_at IS NULL AND expires_at>now()`, user, digest(browser), flowID, digest(cookieValue(r, browserCookie)))
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		http.Error(w, "expired flow", 403)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `UPDATE oauth_flows SET consumed_at=now() WHERE browser_hash=$1 AND consumed_at IS NULL`, digest(cookieValue(r, browserCookie))); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO browser_sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '8 hours')`, digest(session), user); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'account.login','success',$2)`, user, flowID); e != nil || tx.Commit() != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	setCookie(w, browserCookie, browser, 86400)
	setCookie(w, sessionCookie, session, 8*3600)
	http.Redirect(w, r, "/oauth/authorize?flow="+flowID, 303)
}
