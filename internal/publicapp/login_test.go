package publicapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

// The fixture signs ID tokens and enforces PKCE and client authentication at
// the token endpoint; the app uses its production discovery/JWKS verifier.
func testLoginProvider(t *testing.T) (*httptest.Server, context.Context) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "test-key"))
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	codes := map[string]url.Values{}
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
		case "/authorize":
			q := r.URL.Query()
			if q.Get("client_id") != "service-client" || q.Get("scope") != "openid" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("redirect_uri") != "https://app.test/account/callback" {
				http.Error(w, "bad authorize", 400)
				return
			}
			code := randomToken()
			mu.Lock()
			codes[code] = q
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"code": code})
		case "/token":
			r.ParseForm()
			client, secret, ok := r.BasicAuth()
			mu.Lock()
			q, found := codes[r.PostForm.Get("code")]
			delete(codes, r.PostForm.Get("code"))
			mu.Unlock()
			if !ok || client != "service-client" || secret != "fixture-secret" || !found || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != q.Get("redirect_uri") || challenge(r.PostForm.Get("code_verifier")) != q.Get("code_challenge") {
				http.Error(w, "invalid grant", 400)
				return
			}
			claims := map[string]any{"iss": server.URL, "sub": q.Get("test_subject"), "aud": "service-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": q.Get("nonce")}
			switch q.Get("test_invalid") {
			case "nonce":
				claims["nonce"] = "wrong"
			case "issuer":
				claims["iss"] = "https://wrong.test"
			case "audience":
				claims["aud"] = "wrong"
			case "expiry":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			case "subject":
				claims["sub"] = ""
			case "azp":
				claims["azp"] = "wrong"
			case "multi_audience":
				claims["aud"] = []string{"service-client", "other"}
			}
			b, _ := json.Marshal(claims)
			signed, _ := signer.Sign(b)
			raw, _ := signed.CompactSerialize()
			if q.Get("test_invalid") == "signature" {
				parts := strings.Split(raw, ".")
				parts[2] = strings.Repeat("A", len(parts[2]))
				raw = strings.Join(parts, ".")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-unused", "token_type": "Bearer", "expires_in": 3600, "id_token": raw})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, context.WithValue(context.Background(), oauth2.HTTPClient, server.Client())
}

func pageForm(t *testing.T, w *httptest.ResponseRecorder) url.Values {
	t.Helper()
	form := url.Values{}
	for _, name := range []string{"flow", "csrf"} {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(m) != 2 {
			t.Fatalf("missing %s in page, status %d", name, w.Code)
		}
		form.Set(name, m[1])
	}
	return form
}

func newBrowserFlow(t *testing.T, a *App, cookies []*http.Cookie) (*httptest.ResponseRecorder, []*http.Cookie, url.Values) {
	t.Helper()
	q := url.Values{"client_id": {"chatgpt"}, "redirect_uri": {"https://chatgpt.com/connector_platform_oauth_redirect"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {challenge(randomToken())}, "state": {"test-state"}, "resource": {a.resource()}, "scope": {scope}}
	w := request(a, "GET", "/oauth/authorize?"+q.Encode(), "", "", cookies, "")
	if w.Code != 200 {
		t.Fatalf("authorize: %d", w.Code)
	}
	return w, mergeCookies(cookies, w.Result().Cookies()), pageForm(t, w)
}

func mergeCookies(old, updates []*http.Cookie) []*http.Cookie {
	all := map[string]*http.Cookie{}
	for _, c := range old {
		all[c.Name] = c
	}
	for _, c := range updates {
		if c.MaxAge < 0 {
			delete(all, c.Name)
		} else {
			all[c.Name] = c
		}
	}
	out := make([]*http.Cookie, 0, len(all))
	for _, c := range all {
		out = append(out, c)
	}
	return out
}

func loginResponse(t *testing.T, a *App, form url.Values, cookies []*http.Cookie, subject, invalid string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	start := request(a, "POST", "/account/login", form.Encode(), "", cookies, a.cfg.Issuer)
	if start.Code != 303 {
		t.Fatalf("login start: %d %s", start.Code, start.Body)
	}
	u, _ := url.Parse(start.Header().Get("Location"))
	q := u.Query()
	q.Set("test_subject", subject)
	q.Set("test_invalid", invalid)
	u.RawQuery = q.Encode()
	resp, e := a.login.client.Get(u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	var issued struct {
		Code string `json:"code"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&issued); e != nil {
		t.Fatal(e)
	}
	path := "/account/callback?" + url.Values{"state": {q.Get("state")}, "code": {issued.Code}}.Encode()
	return path, request(a, "GET", path, "", "", cookies, "")
}

func loggedInBrowser(t *testing.T, a *App, subject string) ([]*http.Cookie, url.Values) {
	t.Helper()
	_, cookies, form := newBrowserFlow(t, a, nil)
	_, cb := loginResponse(t, a, form, cookies, subject, "")
	if cb.Code != 303 {
		t.Fatalf("login callback: %d %s", cb.Code, cb.Body)
	}
	cookies = mergeCookies(cookies, cb.Result().Cookies())
	w := request(a, "GET", cb.Header().Get("Location"), "", "", cookies, "")
	if w.Code != 200 {
		t.Fatalf("login page: %d %s", w.Code, w.Body)
	}
	return cookies, pageForm(t, w)
}

func bindTestKey(t *testing.T, a *App, cookies []*http.Cookie, form url.Values, key string) url.Values {
	t.Helper()
	form.Set("access_key", key)
	form.Set("days", "7")
	form.Set("confirm", "bind")
	w := request(a, "POST", "/bind/key", form.Encode(), "", cookies, a.cfg.Issuer)
	if w.Code != 303 {
		t.Fatalf("bind key: %d %s", w.Code, w.Body)
	}
	page := request(a, "GET", w.Header().Get("Location"), "", "", cookies, "")
	if page.Code != 200 || strings.Contains(page.Body.String(), key) {
		t.Fatal("binding page failed or exposed key")
	}
	return pageForm(t, page)
}

func TestPostgresOIDCRejectsInvalidTokensAndReplay(t *testing.T) {
	a := testApp(t)
	for _, invalid := range []string{"nonce", "issuer", "audience", "expiry", "subject", "signature", "azp", "multi_audience"} {
		t.Run(invalid, func(t *testing.T) {
			_, cookies, form := newBrowserFlow(t, a, nil)
			path, w := loginResponse(t, a, form, cookies, "alice", invalid)
			if w.Code != 403 || len(w.Result().Cookies()) != 0 {
				t.Fatalf("invalid token accepted: %d", w.Code)
			}
			if request(a, "GET", path, "", "", cookies, "").Code != 403 {
				t.Fatal("failed callback replay accepted")
			}
		})
	}
	var count int
	a.store.DB.QueryRow(`SELECT count(*) FROM app_users`).Scan(&count)
	if count != 0 {
		t.Fatal("invalid login created user")
	}
}
