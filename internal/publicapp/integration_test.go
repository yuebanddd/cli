package publicapp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
	"github.com/Pippit-dev/pippit-cli/internal/common"
)

type verifiedUpstream struct{}

func (verifiedUpstream) Verify(_ context.Context, p auth.RemoteAccessKeyPayload) (string, error) {
	if p.AccessKey != "key-for-"+p.UID {
		return "", errors.New("forged UID")
	}
	return p.UID, nil
}
func testApp(t *testing.T) *App {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for real PostgreSQL integration; CI runs this gate")
	}
	ctx := context.Background()
	admin, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "test_" + strings.ReplaceAll(uuid(), "-", "")
	if _, e = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = admin.Close()
	})
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	key := bytes.Repeat([]byte{7}, 32)
	s, e := Open(ctx, u.String(), key)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal("migration not idempotent", e)
	}
	c := Config{Issuer: "https://app.test", Listen: "127.0.0.1:8787", DatabaseURL: u.String(), Key: key, Clients: map[string][]string{"chatgpt": {"https://chatgpt.com/connector_platform_oauth_redirect"}}, AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, CacheDir: filepath.Join(t.TempDir(), "media"), CacheTTL: 6 * time.Hour, CacheBytes: 1 << 30, MinFreeBytes: 0, MaxFileBytes: 1 << 20, MaxFiles: 12, GlobalConcurrent: 16, UserActiveJobs: 3, NodeBinary: "node"}
	a, e := New(ctx, c, s, verifiedUpstream{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func testUser(t *testing.T, a *App, name string) (string, tokenResponse) {
	t.Helper()
	ctx := context.Background()
	user := uuid()
	if _, e := a.store.DB.ExecContext(ctx, `INSERT INTO app_users(id) VALUES($1)`, user); e != nil {
		t.Fatal(e)
	}
	device, _ := auth.NewRemoteDeviceID()
	c := &auth.Credential{Version: 1, UID: name, DeviceID: device, TokenID: "token_" + name, AccessKey: "key-for-" + name, CredentialScope: "scope-" + name, ExpiredAt: time.Now().Add(time.Hour).Unix()}
	tx, e := a.store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.store.saveCredential(ctx, tx, user, c); e != nil {
		t.Fatal(e)
	}
	family := randomToken()
	expiry := time.Now().Add(time.Hour)
	if _, e = tx.ExecContext(ctx, `INSERT INTO oauth_families(id,user_id,client_id,resource,scope,expires_at) VALUES($1,$2,'chatgpt',$3,$4,$5)`, family, user, a.resource(), scope, expiry); e != nil {
		t.Fatal(e)
	}
	tokens, e := a.issueTokens(ctx, tx, family, expiry)
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	return user, tokens
}
func policyFor(t *testing.T, a *App, token string) *requestPolicy {
	t.Helper()
	p, e := a.authenticate(context.Background(), "Bearer "+token)
	if e != nil {
		t.Fatal(e)
	}
	_, account, e := a.store.resolveCredential(context.Background(), p.UserID)
	if e != nil {
		t.Fatal(e)
	}
	return &requestPolicy{a, p, account, randomToken()}
}
func request(a *App, method, path, body, token string, cookies []*http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://app.test"+path, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.20:45678"
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func mcpRequest(a *App, token, method string, params any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "https://app.test/mcp", bytes.NewReader(b))
	r.RemoteAddr = "203.0.113.20:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func TestPostgresOAuthBrowserPKCEAndReplay(t *testing.T) {
	a := testApp(t)
	verifier := randomToken()
	q := url.Values{"client_id": {"chatgpt"}, "redirect_uri": {"https://chatgpt.com/connector_platform_oauth_redirect"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {challenge(verifier)}, "state": {"state-secret"}, "resource": {a.resource()}, "scope": {scope}}
	w := request(a, "GET", "/oauth/authorize?"+q.Encode(), "", "", nil, "")
	if w.Code != 200 {
		t.Fatalf("authorize: %d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	extract := func(name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(m) != 2 {
			t.Fatalf("missing %s", name)
		}
		return m[1]
	}
	flowID, csrfValue := extract("flow"), extract("csrf")
	form := url.Values{"flow": {flowID}, "csrf": {csrfValue}}
	start := request(a, "POST", "/bind/start", form.Encode(), "", cookies, a.cfg.Issuer)
	if start.Code != 303 {
		t.Fatalf("bind start: %d %s", start.Code, start.Body)
	}
	login, _ := url.Parse(start.Header().Get("Location"))
	callback, _ := url.Parse(login.Query().Get("callback"))
	payload := auth.RemoteAccessKeyPayload{Type: "access_key", UID: "alice", TokenID: "token_alice", AccessKey: "key-for-alice", ExpiredAt: time.Now().Add(time.Hour).Unix(), RandomSecretKey: login.Query().Get("random_secret_key"), Source: auth.RemoteLoginSource, CallbackURL: callback.String()}
	callCallback := func(p auth.RemoteAccessKeyPayload) *httptest.ResponseRecorder {
		b, _ := json.Marshal(p)
		r := httptest.NewRequest("POST", callback.String(), bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://xyq.jianying.com")
		r.RemoteAddr = "203.0.113.20:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	forged := payload
	forged.UID = "victim"
	if w := callCallback(forged); w.Code != 403 {
		t.Fatalf("forged UID accepted: %d %s", w.Code, w.Body)
	}
	if w := callCallback(payload); w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	if w := callCallback(payload); w.Code != 400 {
		t.Fatal("callback replay accepted")
	}
	form.Set("decision", "allow")
	consent := request(a, "POST", "/oauth/consent", form.Encode(), "", cookies, a.cfg.Issuer)
	if consent.Code != 303 {
		t.Fatalf("consent: %d %s", consent.Code, consent.Body)
	}
	redirect, _ := url.Parse(consent.Header().Get("Location"))
	if redirect.Query().Get("state") != "state-secret" || redirect.Query().Get("iss") != a.cfg.Issuer {
		t.Fatal("OAuth response binding missing")
	}
	f := url.Values{"grant_type": {"authorization_code"}, "client_id": {"chatgpt"}, "resource": {a.resource()}, "redirect_uri": {q.Get("redirect_uri")}, "code": {redirect.Query().Get("code")}, "code_verifier": {randomToken()}}
	if _, e := a.exchangeCode(context.Background(), f); e == nil {
		t.Fatal("wrong PKCE accepted")
	}
	f.Set("code_verifier", verifier)
	tokens, e := a.exchangeCode(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.exchangeCode(context.Background(), f); e == nil {
		t.Fatal("code replay accepted")
	}
	if _, e = a.authenticate(context.Background(), "Bearer "+tokens.AccessToken); e != nil {
		t.Fatal(e)
	}
	refresh := url.Values{"client_id": {"chatgpt"}, "resource": {a.resource()}, "refresh_token": {tokens.RefreshToken}}
	rotated, e := a.refresh(context.Background(), refresh)
	if e != nil || rotated.RefreshToken == tokens.RefreshToken {
		t.Fatal("rotation failed", e)
	}
	if _, e = a.refresh(context.Background(), refresh); e == nil {
		t.Fatal("refresh replay accepted")
	}
	if _, e = a.authenticate(context.Background(), "Bearer "+rotated.AccessToken); e == nil {
		t.Fatal("replayed family not revoked")
	}
}
func TestPostgresTwoTenantMCPAndIdempotency(t *testing.T) {
	a := testApp(t)
	userA, tokensA := testUser(t, a, "alice")
	_, tokensB := testUser(t, a, "bob")
	t.Setenv("XYQ_ACCESS_KEY", "GLOBAL-MUST-NEVER-BE-USED")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer key-for-alice" {
			t.Errorf("wrong upstream credential %q", r.Header.Get("Authorization"))
		}
		if strings.HasSuffix(r.URL.Path, "submit_run") {
			io.WriteString(w, `{"ret":"0","data":{"run":{"thread_id":"ta","run_id":"ra"},"web_thread_link":"https://xyq.jianying.com/thread/ta"}}`)
			return
		}
		io.WriteString(w, `{"ret":"0","data":{"readable_text":"working","thread":{"thread_id":"ta","runs":[]}}}`)
	}))
	defer upstream.Close()
	a.clientFactory = func(auth common.RequestAuthorizer) common.Client {
		return common.NewHTTPClient(upstream.URL, time.Second, auth)
	}
	if w := mcpRequest(a, "", "tools/list", map[string]any{}); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatal("missing OAuth challenge")
	}
	tools := mcpRequest(a, tokensA.AccessToken, "tools/list", map[string]any{})
	if tools.Code != 200 || strings.Contains(tools.Body.String(), "pippit_download_result") || strings.Contains(tools.Body.String(), "pippit_auth_status") || !strings.Contains(tools.Body.String(), "pippit_account_status") {
		t.Fatalf("bad public tools: %d %s", tools.Code, tools.Body)
	}
	args := map[string]any{"prompt": "coffee ad", "duration_sec": 10, "ratio": "9:16", "idempotency_key": "coffee-video-001"}
	params := map[string]any{"name": "pippit_generate_video", "arguments": args}
	for i := 0; i < 2; i++ {
		w := mcpRequest(a, tokensA.AccessToken, "tools/call", params)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), "ta") {
			t.Fatalf("generation: %d %s", w.Code, w.Body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("charged %d times", calls.Load())
	}
	for _, query := range []map[string]any{{"name": "pippit_get_thread", "arguments": map[string]any{"thread_id": "ta"}}, {"name": "pippit_nest_submit", "arguments": map[string]any{"thread_id": "ta", "message": "modify", "idempotency_key": "bob-modify-001"}}, {"name": "pippit_query_result", "arguments": map[string]any{"thread_id": "ta", "run_id": "ra"}}} {
		w := mcpRequest(a, tokensB.AccessToken, "tools/call", query)
		if !strings.Contains(w.Body.String(), "resource_not_found") {
			t.Fatalf("cross-tenant access: %s", w.Body)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unauthorized request reached upstream")
	}
	p := policyFor(t, a, tokensA.AccessToken)
	if e := p.owns(context.Background(), resourceRef{"run", "ra", "wrong-thread"}); e == nil {
		t.Fatal("cross-thread run accepted")
	}
	args["prompt"] = "changed"
	w := mcpRequest(a, tokensA.AccessToken, "tools/call", params)
	if !strings.Contains(w.Body.String(), "idempotency_conflict") {
		t.Fatal("changed retry accepted", w.Body)
	}
	var raw []byte
	if e := a.store.DB.QueryRow(`SELECT ciphertext FROM xyq_credentials c JOIN xyq_accounts x ON x.id=c.account_id WHERE x.user_id=$1`, userA).Scan(&raw); e != nil || bytes.Contains(raw, []byte("key-for-alice")) {
		t.Fatal("credential plaintext", e)
	}
	status := mcpRequest(a, tokensA.AccessToken, "tools/call", map[string]any{"name": "pippit_account_status", "arguments": map[string]any{}})
	for _, secret := range []string{"key-for-alice", "token_alice", "credential_scope", "uid"} {
		if strings.Contains(status.Body.String(), secret) {
			t.Fatal("status leaks", secret)
		}
	}
}
func TestPostgresConcurrentRetryAndUncertain(t *testing.T) {
	a := testApp(t)
	_, tokens := testUser(t, a, "alice")
	p := policyFor(t, a, tokens.AccessToken)
	ctx := context.Background()
	args := []byte(`{"idempotency_key":"parallel-1"}`)
	entered := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, e := p.Execute(ctx, "pippit_generate_video", false, args, func(context.Context) ([]byte, error) {
			calls.Add(1)
			close(entered)
			<-finish
			return []byte(`{"thread_id":"t","run_id":"r"}`), nil
		})
		if e != nil {
			t.Error(e)
		}
	}()
	<-entered
	if _, e := p.Execute(ctx, "pippit_generate_video", false, args, func(context.Context) ([]byte, error) { calls.Add(1); return []byte(`{}`), nil }); e == nil {
		t.Error("parallel duplicate not blocked")
	}
	close(finish)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate upstream execution")
	}
	uncertain := []byte(`{"idempotency_key":"uncertain-1"}`)
	for i := 0; i < 2; i++ {
		_, _ = p.Execute(ctx, "pippit_generate_video", false, uncertain, func(context.Context) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New("connection reset after upstream accepted")
		})
	}
	if calls.Load() != 2 {
		t.Fatal("uncertain operation repeated")
	}
}
func TestPostgresExpiryRateLimitsDisconnectAndMigrations(t *testing.T) {
	a := testApp(t)
	user, tokens := testUser(t, a, "alice")
	ctx := context.Background()
	p := policyFor(t, a, tokens.AccessToken)
	release, e := p.acquire(ctx, "pippit_upload_media", false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.acquire(ctx, "pippit_upload_media", false); e == nil {
		t.Fatal("per-user concurrency bypass")
	}
	release()
	for i := 0; i < 3; i++ {
		ok, e := a.store.Allow(ctx, "test-bucket", 2, time.Minute)
		if e != nil || ok != (i < 2) {
			t.Fatal("rate limit", i, e)
		}
	}
	if _, e = a.store.DB.Exec(`UPDATE oauth_tokens SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, digest(tokens.AccessToken)); e != nil {
		t.Fatal(e)
	}
	if _, e = a.authenticate(ctx, "Bearer "+tokens.AccessToken); e == nil {
		t.Fatal("expired access accepted")
	}
	if e = a.unlinkUser(ctx, user); e != nil {
		t.Fatal(e)
	}
	if _, e = a.store.credential(ctx, user); e == nil {
		t.Fatal("credential remains after disconnect")
	}
	if _, e = a.refresh(ctx, url.Values{"client_id": {"chatgpt"}, "resource": {a.resource()}, "refresh_token": {tokens.RefreshToken}}); e == nil {
		t.Fatal("refresh remains valid after disconnect")
	}
	if e = a.store.Cleanup(ctx); e != nil {
		t.Fatal(e)
	}
	if e = a.store.MigrateDown(ctx); e != nil {
		t.Fatal(e)
	}
	if e = a.store.Ready(ctx); e == nil {
		t.Fatal("ready without schema")
	}
	if e = a.store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
}

func TestPostgresBrowserCancelAndLogout(t *testing.T) {
	a := testApp(t)
	user, tokens := testUser(t, a, "alice")
	browser, session := randomToken(), randomToken()
	cookies := []*http.Cookie{{Name: browserCookie, Value: browser}, {Name: sessionCookie, Value: session}}
	if _, e := a.store.DB.Exec(`INSERT INTO browser_sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, digest(session), user); e != nil {
		t.Fatal(e)
	}
	newFlow := func(user string) (string, url.Values) {
		t.Helper()
		id := randomToken()
		data, _ := json.Marshal(authorization{ClientID: "chatgpt", RedirectURI: "https://chatgpt.com/connector_platform_oauth_redirect", Challenge: challenge(randomToken()), State: "cancel-state", Resource: a.resource(), Scope: scope})
		if _, e := a.store.DB.Exec(`INSERT INTO oauth_flows(id,browser_hash,user_id,data,expires_at) VALUES($1,$2,NULLIF($3,'')::uuid,$4,now()+interval '10 minutes')`, id, digest(browser), user, data); e != nil {
			t.Fatal(e)
		}
		return id, url.Values{"flow": {id}, "csrf": {digest("csrf:" + browser + ":" + id)}}
	}
	_, cancelForm := newFlow("")
	cancelForm.Set("decision", "deny")
	w := request(a, "POST", "/oauth/consent", cancelForm.Encode(), "", cookies, a.cfg.Issuer)
	redirect, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != 303 || redirect.Query().Get("error") != "access_denied" || redirect.Query().Get("state") != "cancel-state" || redirect.Query().Get("iss") != a.cfg.Issuer {
		t.Fatalf("anonymous cancellation failed: %d %s", w.Code, w.Body)
	}
	if replay := request(a, "POST", "/oauth/consent", cancelForm.Encode(), "", cookies, a.cfg.Issuer); replay.Code != 403 {
		t.Fatal("cancelled flow reused")
	}
	_, logoutForm := newFlow(user)
	otherID, otherForm := newFlow(user)
	w = request(a, "POST", "/account/logout", logoutForm.Encode(), "", cookies, a.cfg.Issuer)
	if w.Code != 200 {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	otherForm.Set("decision", "allow")
	if w = request(a, "POST", "/oauth/consent", otherForm.Encode(), "", cookies, a.cfg.Issuer); w.Code != 403 {
		t.Fatal("old tab authorized after logout")
	}
	if w = request(a, "GET", "/oauth/authorize?flow="+otherID, "", "", cookies, ""); w.Code != 400 {
		t.Fatal("old tab restored after logout")
	}
	r := httptest.NewRequest("GET", "https://app.test", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if a.session(r) != "" {
		t.Fatal("session survived logout")
	}
	if _, e := a.authenticate(context.Background(), "Bearer "+tokens.AccessToken); e != nil {
		t.Fatal("browser logout unexpectedly disconnected OAuth", e)
	}
}

func TestPostgresInFlightRevocationAndMetadataRetention(t *testing.T) {
	a := testApp(t)
	_, tokens := testUser(t, a, "alice")
	p := policyFor(t, a, tokens.AccessToken)
	runner := p.runner()
	ctx := context.Background()
	if _, e := runner.Auth.ResolveAccessKey(ctx); e != nil {
		t.Fatal(e)
	}
	_, e := p.Execute(ctx, "pippit_generate_video", false, []byte(`{"idempotency_key":"retention-1"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"thread_id":"retention-t","run_id":"retention-r"}`), nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.store.DB.Exec(`UPDATE jobs SET updated_at=now()-interval '31 days',result_metadata='{"url":"https://example.test/expired"}'`); e != nil {
		t.Fatal(e)
	}
	if e = a.store.Cleanup(ctx); e != nil {
		t.Fatal(e)
	}
	var retained bool
	if e = a.store.DB.QueryRow(`SELECT response IS NULL AND result_metadata IS NULL FROM jobs`).Scan(&retained); e != nil || !retained {
		t.Fatal("URL metadata retention failed", e)
	}
	if _, e = a.store.DB.Exec(`UPDATE oauth_families SET revoked_at=now() WHERE id=$1`, p.principal.FamilyID); e != nil {
		t.Fatal(e)
	}
	if _, e = runner.Auth.ResolveAccessKey(ctx); e == nil {
		t.Fatal("in-flight runner used credential after family revocation")
	}
}

func TestPostgresMCPResultMediaNeverDownloaded(t *testing.T) {
	a := testApp(t)
	_, tokens := testUser(t, a, "alice")
	p := policyFor(t, a, tokens.AccessToken)
	_, e := p.Execute(context.Background(), "pippit_generate_video", false, []byte(`{"idempotency_key":"remote-result-1"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"thread_id":"result-thread","run_id":"result-run"}`), nil
	})
	if e != nil {
		t.Fatal(e)
	}
	var mediaRequests atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaRequests.Add(1)
		t.Error("generated media was fetched", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer media.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-for-alice" {
			t.Error("wrong tenant credential")
		}
		content := []any{
			map[string]any{"sub_type": "biz/x_data_video", "data": map[string]any{"video": map[string]any{"download_url": media.URL + "/video.mp4", "asset_id": "result-video", "vid": "video-id", "title": "Generated video"}}},
			map[string]any{"sub_type": "biz/x_data_image", "data": map[string]any{"image": map[string]any{"url": media.URL + "/image.png", "asset_id": "result-image", "metadata": map[string]any{"format": "png"}}}},
		}
		writeJSON(w, 200, map[string]any{"ret": "0", "data": map[string]any{"thread": map[string]any{"thread_id": "result-thread", "run_list": []any{map[string]any{"run_id": "result-run", "state": 3, "entry_list": []any{map[string]any{"artifact": map[string]any{"content": content}}}}}}}})
	}))
	defer upstream.Close()
	a.clientFactory = func(auth common.RequestAuthorizer) common.Client {
		return common.NewHTTPClient(upstream.URL, time.Second, auth)
	}
	w := mcpRequest(a, tokens.AccessToken, "tools/call", map[string]any{"name": "pippit_query_result", "arguments": map[string]any{"thread_id": "result-thread", "run_id": "result-run"}})
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), media.URL+"/video.mp4") || !strings.Contains(w.Body.String(), media.URL+"/image.png") || strings.Contains(w.Body.String(), "output_path") {
		t.Fatalf("remote metadata missing: %d %s", w.Code, w.Body)
	}
	if mediaRequests.Load() != 0 {
		t.Fatal("generated media passed through server")
	}
	entries, e := os.ReadDir(a.cache.Dir)
	if e != nil || len(entries) != 0 {
		t.Fatal("result query wrote to cache", e)
	}
	var finished bool
	var metadata []byte
	if e = a.store.DB.QueryRow(`SELECT generation_finished,result_metadata FROM jobs`).Scan(&finished, &metadata); e != nil || !finished || !bytes.Contains(metadata, []byte(media.URL+"/video.mp4")) {
		t.Fatal("result metadata not persisted", e)
	}
}

func TestPostgresPublicCanvasRuntimeAndStateIsolation(t *testing.T) {
	a := testApp(t)
	script, e := filepath.Abs("../../scripts/public-canvas-command.js")
	if e != nil {
		t.Fatal(e)
	}
	a.cfg.CanvasScript = script
	_, tokensA := testUser(t, a, "alice")
	_, tokensB := testUser(t, a, "bob")
	p := policyFor(t, a, tokensA.AccessToken)
	ctx := context.Background()
	output, e := p.RunCanvasCommand(ctx, []string{"list"})
	if e != nil || !bytes.Contains(output, []byte("commands")) {
		t.Fatal("public runtime catalog unavailable; run npm run prepare:canvas-runtime", e)
	}
	tx, e := a.store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = p.remember(ctx, tx, []resourceRef{{"asset", "canvas-a", ""}}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, e = p.canvasOperation(ctx, "canvas-a", "state.write", []byte(`{"state":{"clientId":"alice-private","outbound":[]}}`)); e != nil {
		t.Fatal(e)
	}
	state, e := p.canvasOperation(ctx, "canvas-a", "state.read", []byte(`{}`))
	encoded, _ := json.Marshal(state)
	if e != nil || !bytes.Contains(encoded, []byte("alice-private")) {
		t.Fatal("Canvas state missing", e)
	}
	b := policyFor(t, a, tokensB.AccessToken)
	if _, e = b.RunCanvasCommand(ctx, []string{"run", "list_checkpoints", "--canvas-id", "canvas-a", "--input", "{}"}); e == nil {
		t.Fatal("foreign Canvas accepted by Node bridge")
	}
	state, e = b.canvasOperation(ctx, "canvas-a", "state.read", []byte(`{}`))
	encoded, _ = json.Marshal(state)
	if e == nil && bytes.Contains(encoded, []byte("alice-private")) {
		t.Fatal("foreign Canvas state exposed")
	}
}

func TestPostgresReadCannotClaimForeignThread(t *testing.T) {
	a := testApp(t)
	_, tokens := testUser(t, a, "alice")
	p := policyFor(t, a, tokens.AccessToken)
	ctx := context.Background()
	_, e := p.Execute(ctx, "pippit_generate_video", false, []byte(`{"idempotency_key":"owned-thread-1"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"thread_id":"owned","run_id":"original"}`), nil
	})
	if e != nil {
		t.Fatal(e)
	}
	_, e = p.Execute(ctx, "pippit_get_thread", true, []byte(`{"thread_id":"owned"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"thread_id":"owned","data":{"thread":{"thread_id":"foreign","run_list":[{"run_id":"foreign-run"}]}}}`), nil
	})
	if e == nil || p.owns(ctx, resourceRef{"thread", "foreign", ""}) == nil {
		t.Fatal("read response acquired a foreign thread")
	}
	_, e = p.Execute(ctx, "pippit_get_thread", true, []byte(`{"thread_id":"owned"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"thread_id":"owned","data":{"thread":{"thread_id":"owned","run_list":[{"run_id":"discovered"}]}}}`), nil
	})
	if e != nil || p.owns(ctx, resourceRef{"run", "discovered", "owned"}) != nil {
		t.Fatal("nested run not associated with its owned thread", e)
	}
}
