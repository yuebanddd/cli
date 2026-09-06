package publicapp

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func flowUser(t *testing.T, a *App, form url.Values) string {
	t.Helper()
	var user string
	if e := a.store.DB.QueryRow(`SELECT user_id FROM oauth_flows WHERE id=$1`, form.Get("flow")).Scan(&user); e != nil {
		t.Fatal(e)
	}
	return user
}

func tokensForUser(t *testing.T, a *App, user string) tokenResponse {
	t.Helper()
	tx, e := a.store.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	family := randomToken()
	expiry := time.Now().Add(time.Hour)
	if _, e = tx.Exec(`INSERT INTO oauth_families(id,user_id,client_id,resource,scope,expires_at) VALUES($1,$2,'chatgpt',$3,$4,$5)`, family, user, a.resource(), scope, expiry); e != nil {
		t.Fatal(e)
	}
	tokens, e := a.issueTokens(context.Background(), tx, family, expiry)
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	return tokens
}

func TestPostgresManualBindingIsolationAndRotation(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	cookiesA, formA := loggedInBrowser(t, a, "alice")
	userA := flowUser(t, a, formA)
	_, again := loggedInBrowser(t, a, "alice")
	if flowUser(t, a, again) != userA {
		t.Fatal("same OIDC identity did not recover service account")
	}
	cookiesB, formB := loggedInBrowser(t, a, "bob")
	userB := flowUser(t, a, formB)
	if userA == userB {
		t.Fatal("OIDC identities merged")
	}
	if request(a, "POST", "/bind/key", formA.Encode(), "", cookiesB, a.cfg.Issuer).Code != 403 {
		t.Fatal("foreign browser accepted")
	}
	formA = bindTestKey(t, a, cookiesA, formA, "fixture-shared-key")
	formB = bindTestKey(t, a, cookiesB, formB, "fixture-shared-key")
	_, accountA, e := a.store.resolveCredential(ctx, userA)
	if e != nil {
		t.Fatal(e)
	}
	_, accountB, e := a.store.resolveCredential(ctx, userB)
	if e != nil || accountA == accountB {
		t.Fatal("manual key merged accounts", e)
	}
	var encrypted []byte
	var source string
	if e = a.store.DB.QueryRow(`SELECT c.ciphertext,a.binding_source FROM xyq_credentials c JOIN xyq_accounts a ON a.id=c.account_id WHERE a.id=$1`, accountA).Scan(&encrypted, &source); e != nil || bytes.Contains(encrypted, []byte("fixture-shared-key")) || source != "manual_unverified" {
		t.Fatal("credential encryption or provenance incorrect", e)
	}
	tokens := tokensForUser(t, a, userA)
	p := policyFor(t, a, tokens.AccessToken)
	for i := 0; i < a.cfg.UserActiveJobs; i++ {
		_, e = p.Execute(ctx, "pippit_generate_video", false, []byte(`{"idempotency_key":"rotation-job-`+string(rune('a'+i))+`"}`), func(context.Context) ([]byte, error) {
			return []byte(`{"thread_id":"old-thread-` + string(rune('a'+i)) + `","run_id":"old-run-` + string(rune('a'+i)) + `"}`), nil
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	oldForm := formA
	formA = bindTestKey(t, a, cookiesA, formA, "fixture-new-key")
	if request(a, "POST", "/bind/key", oldForm.Encode(), "", cookiesA, a.cfg.Issuer).Code != 403 {
		t.Fatal("binding replay accepted")
	}
	if _, e = a.authenticate(ctx, "Bearer "+tokens.AccessToken); e == nil {
		t.Fatal("rotation retained old OAuth family")
	}
	if _, e = p.runner().Auth.ResolveAccessKey(ctx); e == nil {
		t.Fatal("old runner acquired replacement credential")
	}
	newTokens := tokensForUser(t, a, userA)
	newPolicy := policyFor(t, a, newTokens.AccessToken)
	if newPolicy.accountID == accountA {
		t.Fatal("rotation reused account")
	}
	if e = newPolicy.owns(ctx, resourceRef{kind: "thread", id: "old-thread-a"}); e == nil {
		t.Fatal("rotation inherited old resources")
	}
	job, e := newPolicy.GetJob(ctx, "rotation-job-a")
	if e != nil || job.Found {
		t.Fatal("rotation inherited old job", e)
	}
	if _, e = newPolicy.Execute(ctx, "pippit_generate_video", false, []byte(`{"idempotency_key":"rotation-new-job"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"thread_id":"new-thread","run_id":"new-run"}`), nil
	}); e != nil {
		t.Fatal("old account jobs exhausted new binding capacity", e)
	}
	var count int
	if e = a.store.DB.QueryRow(`SELECT count(*) FROM xyq_credentials c JOIN xyq_accounts a ON a.id=c.account_id WHERE a.user_id=$1`, userA).Scan(&count); e != nil || count != 1 {
		t.Fatal("old credential retained", e)
	}
	if c, e := a.store.credential(ctx, userB); e != nil || c.AccessKey != "fixture-shared-key" {
		t.Fatal("other tenant credential changed", e)
	}
	formA.Set("confirm", "delete")
	if w := request(a, "POST", "/account/delete", formA.Encode(), "", cookiesA, a.cfg.Issuer); w.Code != 200 {
		t.Fatalf("delete account: %d", w.Code)
	}
	if e = a.store.DB.QueryRow(`SELECT count(*) FROM service_identities WHERE user_id=$1`, userA).Scan(&count); e != nil || count != 0 {
		t.Fatal("identity survived account deletion", e)
	}
}

func TestPostgresBindingRequiresSessionCSRFAndConsent(t *testing.T) {
	a := testApp(t)
	_, anonCookies, anon := newBrowserFlow(t, a, nil)
	anon.Set("access_key", "fixture-key")
	anon.Set("days", "7")
	anon.Set("confirm", "bind")
	if request(a, "POST", "/bind/key", anon.Encode(), "", anonCookies, a.cfg.Issuer).Code != 401 {
		t.Fatal("anonymous binding accepted")
	}
	cookies, form := loggedInBrowser(t, a, "alice")
	form.Set("access_key", "fixture-key")
	form.Set("days", "7")
	form.Set("confirm", "bind")
	if request(a, "POST", "/bind/key", form.Encode(), "", cookies, "https://evil.test").Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	for _, tc := range []struct{ name, value string }{{"csrf", "wrong"}, {"confirm", ""}, {"access_key", "bad\r\nkey"}, {"days", "999"}} {
		copy := url.Values{}
		for k, v := range form {
			copy[k] = v
		}
		copy.Set(tc.name, tc.value)
		w := request(a, "POST", "/bind/key", copy.Encode(), "", cookies, a.cfg.Issuer)
		if w.Code != 400 && w.Code != 403 {
			t.Fatalf("invalid %s accepted: %d", tc.name, w.Code)
		}
	}
	withoutSession := []*http.Cookie{}
	for _, c := range cookies {
		if c.Name != sessionCookie {
			withoutSession = append(withoutSession, c)
		}
	}
	if request(a, "POST", "/bind/key", form.Encode(), "", withoutSession, a.cfg.Issuer).Code != 403 {
		t.Fatal("flow without session accepted")
	}
	for _, path := range []string{"/bind/start", "/bind/callback"} {
		if request(a, "POST", path, form.Encode(), "", cookies, a.cfg.Issuer).Code != 404 {
			t.Fatal("retired callback endpoint still exists")
		}
	}
}

func TestPostgresUpgradeDoesNotPromoteLegacyIdentity(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	if _, e := a.store.DB.Exec(migration002Down); e != nil {
		t.Fatal(e)
	}
	if e := a.store.Ready(ctx); e == nil {
		t.Fatal("old schema passed readiness")
	}
	user, tokens := testUser(t, a, "alice")
	if _, e := a.store.DB.Exec(`INSERT INTO upstream_identities(subject,user_id) VALUES('alice',$1)`, user); e != nil {
		t.Fatal(e)
	}
	if e := a.store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := a.authenticate(ctx, "Bearer "+tokens.AccessToken); e == nil {
		t.Fatal("legacy token survived upgrade")
	}
	_, form := loggedInBrowser(t, a, "alice")
	if flowUser(t, a, form) == user {
		t.Fatal("unrelated OIDC subject claimed legacy upstream user")
	}
}

func TestPostgresCodeIssuanceSerializedWithRebinding(t *testing.T) {
	a := testApp(t)
	cookies, form := loggedInBrowser(t, a, "alice")
	form = bindTestKey(t, a, cookies, form, "fixture-key")
	user := flowUser(t, a, form)
	code, verifier := randomToken(), randomToken()
	redirect := "https://chatgpt.com/connector_platform_oauth_redirect"
	if _, e := a.store.DB.Exec(`INSERT INTO oauth_codes(code_hash,user_id,client_id,redirect_uri,challenge,resource,scope,expires_at) VALUES($1,$2,'chatgpt',$3,$4,$5,$6,now()+interval '1 minute')`, digest(code), user, redirect, challenge(verifier), a.resource(), scope); e != nil {
		t.Fatal(e)
	}
	grant := url.Values{"code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "client_id": {"chatgpt"}, "resource": {a.resource()}}
	tx, e := a.store.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`SELECT id FROM app_users WHERE id=$1 FOR UPDATE`, user); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, e = a.exchangeCode(ctx, grant); e == nil || ctx.Err() == nil {
		t.Fatal("issuance did not wait for account mutation lock", e)
	}
	if e = tx.Rollback(); e != nil {
		t.Fatal(e)
	}
	bindTestKey(t, a, cookies, form, "fixture-new-key")
	if _, e = a.exchangeCode(context.Background(), grant); e == nil {
		t.Fatal("old grant survived rebind")
	}
}
