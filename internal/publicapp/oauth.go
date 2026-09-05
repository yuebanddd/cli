package publicapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const scope = "xiaoyunque:tools"

type authorization struct {
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
	Challenge   string `json:"challenge"`
	State       string `json:"state"`
	Resource    string `json:"resource"`
	Scope       string `json:"scope"`
}
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}
type principal struct{ UserID, FamilyID, ClientID string }

func (a *App) metadata(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/oauth-authorization-server" {
		writeJSON(w, 200, map[string]any{"issuer": a.cfg.Issuer, "authorization_endpoint": a.cfg.Issuer + "/oauth/authorize", "token_endpoint": a.cfg.Issuer + "/oauth/token", "revocation_endpoint": a.cfg.Issuer + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{scope}, "authorization_response_iss_parameter_supported": true})
		return
	}
	writeJSON(w, 200, map[string]any{"resource": a.resource(), "authorization_servers": []string{a.cfg.Issuer}, "scopes_supported": []string{scope}, "bearer_methods_supported": []string{"header"}})
}
func (a *App) validAuthorization(ctx context.Context, q url.Values) (authorization, error) {
	var out authorization
	for _, key := range []string{"client_id", "redirect_uri", "code_challenge", "code_challenge_method", "response_type", "state", "resource", "scope"} {
		if len(q[key]) != 1 {
			return out, errors.New("invalid_request")
		}
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || !pkceValid(q.Get("code_challenge")) || q.Get("resource") != a.resource() || q.Get("scope") != scope || len(q.Get("state")) < 1 || len(q.Get("state")) > 1024 {
		return out, errors.New("invalid_request")
	}
	var raw []byte
	if e := a.store.DB.QueryRowContext(ctx, `SELECT redirect_uris FROM oauth_clients WHERE id=$1`, q.Get("client_id")).Scan(&raw); e != nil {
		return out, errors.New("invalid_client")
	}
	var redirects []string
	if json.Unmarshal(raw, &redirects) != nil {
		return out, errors.New("invalid_client")
	}
	found := false
	for _, u := range redirects {
		if u == q.Get("redirect_uri") {
			found = true
		}
	}
	if !found {
		return out, errors.New("invalid_redirect_uri")
	}
	return authorization{q.Get("client_id"), q.Get("redirect_uri"), q.Get("code_challenge"), q.Get("state"), q.Get("resource"), scope}, nil
}
func (a *App) token(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	for _, k := range []string{"grant_type", "client_id", "resource"} {
		if len(r.PostForm[k]) != 1 {
			oauthError(w, "invalid_request")
			return
		}
	}
	if r.PostForm.Get("resource") != a.resource() {
		oauthError(w, "invalid_target")
		return
	}
	var result tokenResponse
	var err error
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		result, err = a.exchangeCode(r.Context(), r.PostForm)
	case "refresh_token":
		result, err = a.refresh(r.Context(), r.PostForm)
	default:
		oauthError(w, "unsupported_grant_type")
		return
	}
	if err != nil {
		oauthError(w, "invalid_grant")
		return
	}
	writeJSON(w, 200, result)
}
func (a *App) exchangeCode(ctx context.Context, f url.Values) (tokenResponse, error) {
	var empty tokenResponse
	for _, k := range []string{"code", "code_verifier", "redirect_uri"} {
		if len(f[k]) != 1 {
			return empty, errors.New("invalid_grant")
		}
	}
	verifier := f.Get("code_verifier")
	if !pkceValid(verifier) {
		return empty, errors.New("invalid_grant")
	}
	tx, e := a.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback()
	var user, client, redirect, expected, resource, granted string
	var expires time.Time
	var consumed sql.NullTime
	e = tx.QueryRowContext(ctx, `SELECT user_id,client_id,redirect_uri,challenge,resource,scope,expires_at,consumed_at FROM oauth_codes WHERE code_hash=$1 FOR UPDATE`, digest(f.Get("code"))).Scan(&user, &client, &redirect, &expected, &resource, &granted, &expires, &consumed)
	if e != nil || consumed.Valid || !expires.After(time.Now()) || client != f.Get("client_id") || redirect != f.Get("redirect_uri") || resource != f.Get("resource") || !equal(expected, challenge(verifier)) {
		return empty, errors.New("invalid_grant")
	}
	if _, e = tx.ExecContext(ctx, `UPDATE oauth_codes SET consumed_at=now() WHERE code_hash=$1`, digest(f.Get("code"))); e != nil {
		return empty, e
	}
	family := randomToken()
	familyExpires := time.Now().Add(a.cfg.RefreshTTL)
	if _, e = tx.ExecContext(ctx, `INSERT INTO oauth_families(id,user_id,client_id,resource,scope,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, family, user, client, resource, granted, familyExpires); e != nil {
		return empty, e
	}
	result, e := a.issueTokens(ctx, tx, family, familyExpires)
	if e != nil {
		return empty, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'oauth.code','success',$2)`, user, randomToken()); e != nil {
		return empty, e
	}
	return result, tx.Commit()
}
func (a *App) issueTokens(ctx context.Context, tx *sql.Tx, family string, familyExpires time.Time) (tokenResponse, error) {
	result := tokenResponse{randomToken(), "Bearer", int(a.cfg.AccessTTL.Seconds()), randomToken(), scope}
	accessExpires := time.Now().Add(a.cfg.AccessTTL)
	if accessExpires.After(familyExpires) {
		accessExpires = familyExpires
		result.ExpiresIn = int(time.Until(familyExpires).Seconds())
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO oauth_tokens(token_hash,family_id,kind,expires_at) VALUES($1,$2,'access',$3),($4,$2,'refresh',$5)`, digest(result.AccessToken), family, accessExpires, digest(result.RefreshToken), familyExpires)
	return result, e
}
func (a *App) refresh(ctx context.Context, f url.Values) (tokenResponse, error) {
	var empty tokenResponse
	if len(f["refresh_token"]) != 1 || len(f["scope"]) > 1 || (f.Get("scope") != "" && f.Get("scope") != scope) {
		return empty, errors.New("invalid_grant")
	}
	tx, e := a.store.DB.BeginTx(ctx, nil)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback()
	var family, user, client, resource, granted string
	var expires, familyExpires time.Time
	var used, revoked sql.NullTime
	// Lock both rows: parallel rotations serialize; replay revokes the whole family.
	e = tx.QueryRowContext(ctx, `SELECT t.family_id,f.user_id,f.client_id,f.resource,f.scope,t.expires_at,f.expires_at,t.consumed_at,f.revoked_at FROM oauth_tokens t JOIN oauth_families f ON f.id=t.family_id WHERE t.token_hash=$1 AND t.kind='refresh' FOR UPDATE OF f,t`, digest(f.Get("refresh_token"))).Scan(&family, &user, &client, &resource, &granted, &expires, &familyExpires, &used, &revoked)
	if e != nil || client != f.Get("client_id") || resource != f.Get("resource") || granted != scope || revoked.Valid || !expires.After(time.Now()) || !familyExpires.After(time.Now()) {
		return empty, errors.New("invalid_grant")
	}
	if used.Valid {
		if _, e = tx.ExecContext(ctx, `UPDATE oauth_families SET revoked_at=now() WHERE id=$1`, family); e != nil {
			return empty, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'oauth.refresh','replay',$2)`, user, randomToken()); e != nil {
			return empty, e
		}
		if e = tx.Commit(); e != nil {
			return empty, e
		}
		return empty, errors.New("refresh replay")
	}
	if _, e = tx.ExecContext(ctx, `UPDATE oauth_tokens SET consumed_at=now() WHERE token_hash=$1`, digest(f.Get("refresh_token"))); e != nil {
		return empty, e
	}
	result, e := a.issueTokens(ctx, tx, family, familyExpires)
	if e != nil {
		return empty, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO audit_events(user_id,event,outcome,correlation_id) VALUES($1,'oauth.refresh','success',$2)`, user, randomToken()); e != nil {
		return empty, e
	}
	return result, tx.Commit()
}
func (a *App) authenticate(ctx context.Context, header string) (principal, error) {
	var p principal
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) != 43 {
		return p, errors.New("unauthorized")
	}
	e := a.store.DB.QueryRowContext(ctx, `SELECT f.user_id,f.id,f.client_id FROM oauth_tokens t JOIN oauth_families f ON f.id=t.family_id WHERE t.token_hash=$1 AND t.kind='access' AND t.expires_at>now() AND f.expires_at>now() AND f.revoked_at IS NULL AND f.resource=$2 AND f.scope=$3`, digest(parts[1]), a.resource(), scope).Scan(&p.UserID, &p.FamilyID, &p.ClientID)
	return p, e
}
func (a *App) revoke(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	if len(r.PostForm["token"]) != 1 || len(r.PostForm["client_id"]) != 1 {
		oauthError(w, "invalid_request")
		return
	}
	var user string
	e := a.store.DB.QueryRowContext(r.Context(), `SELECT f.user_id FROM oauth_families f JOIN oauth_tokens t ON t.family_id=f.id WHERE f.client_id=$1 AND t.token_hash=$2`, r.PostForm.Get("client_id"), digest(r.PostForm.Get("token"))).Scan(&user)
	if e == nil {
		e = a.unlinkUser(r.Context(), user)
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		http.Error(w, "temporarily unavailable", 503)
		return
	}
	writeJSON(w, 200, map[string]any{})
}
func oauthError(w http.ResponseWriter, code string) {
	writeJSON(w, 400, map[string]string{"error": code})
}
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		http.Error(w, "form content type required", 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		http.Error(w, "invalid form", 400)
		return false
	}
	for _, v := range r.PostForm {
		if len(v) != 1 {
			http.Error(w, "duplicate form field", 400)
			return false
		}
	}
	return true
}
