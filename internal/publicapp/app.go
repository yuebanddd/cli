package publicapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/config"
	"github.com/Pippit-dev/pippit-cli/internal/mcpserver"
)

type Config struct {
	Issuer, Listen, DatabaseURL                   string
	Key                                           []byte
	Clients                                       map[string][]string
	AccessTTL, RefreshTTL                         time.Duration
	CacheDir                                      string
	CacheTTL                                      time.Duration
	CacheBytes, MinFreeBytes, MaxFileBytes        int64
	MaxFiles, GlobalConcurrent, UserActiveJobs    int
	AllowChatGPTFakeIP                            bool
	TrustedProxies                                []netip.Prefix
	NodeBinary, CanvasScript                      string
	LoginIssuer, LoginClientID, LoginClientSecret string
}

func ConfigFromEnv() (Config, error) {
	c := Config{Issuer: strings.TrimRight(os.Getenv("PIPPIT_PUBLIC_ISSUER"), "/"), Listen: "0.0.0.0:8787", DatabaseURL: os.Getenv("DATABASE_URL"), AccessTTL: time.Hour, RefreshTTL: 30 * 24 * time.Hour, CacheDir: os.Getenv("PIPPIT_MEDIA_CACHE_DIR"), CacheTTL: 6 * time.Hour, CacheBytes: 4 << 30, MinFreeBytes: 1 << 30, MaxFileBytes: 200 << 20, MaxFiles: 12, GlobalConcurrent: 16, UserActiveJobs: 3, NodeBinary: "node", CanvasScript: os.Getenv("PIPPIT_PUBLIC_CANVAS_SCRIPT")}
	var e error
	c.LoginIssuer = os.Getenv("PIPPIT_LOGIN_ISSUER")
	c.LoginClientID = os.Getenv("PIPPIT_LOGIN_CLIENT_ID")
	c.LoginClientSecret = os.Getenv("PIPPIT_LOGIN_CLIENT_SECRET")
	c.Key, e = base64.StdEncoding.DecodeString(os.Getenv("PIPPIT_CREDENTIAL_MASTER_KEY"))
	if e != nil || len(c.Key) != 32 {
		return c, errors.New("PIPPIT_CREDENTIAL_MASTER_KEY must be base64 encoding of 32 random bytes")
	}
	if e = json.Unmarshal([]byte(os.Getenv("PIPPIT_PUBLIC_CLIENTS_JSON")), &c.Clients); e != nil {
		return c, errors.New("PIPPIT_PUBLIC_CLIENTS_JSON is required")
	}
	if v := os.Getenv("PIPPIT_MCP_LISTEN"); v != "" {
		c.Listen = v
	}
	for name, dest := range map[string]*time.Duration{"PIPPIT_MEDIA_CACHE_TTL": &c.CacheTTL, "PIPPIT_OAUTH_ACCESS_TTL": &c.AccessTTL, "PIPPIT_OAUTH_REFRESH_TTL": &c.RefreshTTL} {
		if raw := os.Getenv(name); raw != "" {
			*dest, e = time.ParseDuration(raw)
			if e != nil {
				return c, fmt.Errorf("invalid %s", name)
			}
		}
	}
	for name, dest := range map[string]*int64{"PIPPIT_MEDIA_CACHE_MAX_BYTES": &c.CacheBytes, "PIPPIT_MEDIA_MIN_FREE_BYTES": &c.MinFreeBytes, "PIPPIT_MCP_MAX_FILE_BYTES": &c.MaxFileBytes} {
		if raw := os.Getenv(name); raw != "" {
			*dest, e = strconv.ParseInt(raw, 10, 64)
			if e != nil {
				return c, fmt.Errorf("invalid %s", name)
			}
		}
	}
	for name, dest := range map[string]*int{"PIPPIT_MEDIA_MAX_FILES": &c.MaxFiles, "PIPPIT_PUBLIC_GLOBAL_CONCURRENT": &c.GlobalConcurrent, "PIPPIT_PUBLIC_USER_ACTIVE_JOBS": &c.UserActiveJobs} {
		if raw := os.Getenv(name); raw != "" {
			*dest, e = strconv.Atoi(raw)
			if e != nil {
				return c, fmt.Errorf("invalid %s", name)
			}
		}
	}
	if v := os.Getenv("PIPPIT_CHATGPT_ALLOW_FAKE_IP"); v != "" {
		c.AllowChatGPTFakeIP, e = strconv.ParseBool(v)
		if e != nil {
			return c, errors.New("invalid PIPPIT_CHATGPT_ALLOW_FAKE_IP")
		}
	}
	for _, raw := range strings.Split(os.Getenv("PIPPIT_TRUSTED_PROXY_CIDRS"), ",") {
		if raw = strings.TrimSpace(raw); raw != "" {
			p, e := netip.ParsePrefix(raw)
			if e != nil {
				return c, e
			}
			c.TrustedProxies = append(c.TrustedProxies, p)
		}
	}
	if c.CanvasScript == "" {
		return c, errors.New("PIPPIT_PUBLIC_CANVAS_SCRIPT is required for the full public tool surface")
	}
	return c, c.validate()
}
func (c Config) validate() error {
	if !validLoginURL(c.LoginIssuer) || c.LoginClientID == "" || c.LoginClientSecret == "" {
		return errors.New("public mode requires an HTTPS OIDC issuer, client ID and client secret")
	}
	u, e := url.Parse(c.Issuer)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("PIPPIT_PUBLIC_ISSUER must be an HTTPS origin without path, query or userinfo")
	}
	if c.DatabaseURL == "" || len(c.Key) != 32 || c.CacheDir == "" || len(c.Clients) == 0 {
		return errors.New("public mode requires PostgreSQL, key, cache directory and predefined OAuth clients")
	}
	if _, _, e = net.SplitHostPort(c.Listen); e != nil {
		return e
	}
	if c.AccessTTL < time.Minute || c.AccessTTL > time.Hour || c.RefreshTTL < time.Hour || c.RefreshTTL > 90*24*time.Hour || c.GlobalConcurrent < 1 || c.UserActiveJobs < 1 {
		return errors.New("invalid token or concurrency limits")
	}
	for id, redirects := range c.Clients {
		if len(id) < 1 || len(id) > 256 || len(redirects) == 0 {
			return errors.New("invalid OAuth client")
		}
		for _, raw := range redirects {
			r, e := url.Parse(raw)
			if e != nil || r.Scheme != "https" || r.Host == "" || r.User != nil || r.Fragment != "" || strings.Contains(raw, "*") {
				return errors.New("OAuth redirects must be exact HTTPS URLs")
			}
		}
	}
	return nil
}

type App struct {
	cfg           Config
	store         *Store
	login         *serviceLogin
	cache         *mcpserver.MediaCache
	logger        *slog.Logger
	clientFactory func(common.RequestAuthorizer) common.Client
}

func New(ctx context.Context, c Config, s *Store, logger *slog.Logger) (*App, error) {
	if e := c.validate(); e != nil {
		return nil, e
	}
	if s == nil {
		return nil, errors.New("store required")
	}
	if e := s.Ready(ctx); e != nil {
		return nil, errors.New("database migration required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	login, e := newServiceLogin(ctx, c)
	if e != nil {
		return nil, e
	}
	a := &App{cfg: c, store: s, login: login, logger: logger}
	a.clientFactory = func(auth common.RequestAuthorizer) common.Client {
		return common.NewHTTPClient(config.DefaultBaseURL, 2*time.Minute, auth)
	}
	a.cache = &mcpserver.MediaCache{Dir: c.CacheDir, TTL: c.CacheTTL, MaxBytes: c.CacheBytes, MinFreeBytes: c.MinFreeBytes, MaxFileBytes: c.MaxFileBytes, MaxFiles: c.MaxFiles, AllowChatGPTFakeIP: c.AllowChatGPTFakeIP}
	if e := a.cache.Prepare(); e != nil {
		return nil, e
	}
	for id, redirects := range c.Clients {
		b, _ := json.Marshal(redirects)
		if _, e := s.DB.ExecContext(ctx, `INSERT INTO oauth_clients(id,redirect_uris) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET redirect_uris=EXCLUDED.redirect_uris`, id, b); e != nil {
			return nil, e
		}
	}
	return a, nil
}
func (a *App) resource() string { return a.cfg.Issuer + "/mcp" }
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.metadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", a.metadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.metadata)
	mux.HandleFunc("GET /oauth/authorize", a.authorize)
	mux.HandleFunc("POST /oauth/token", a.token)
	mux.HandleFunc("POST /oauth/revoke", a.revoke)
	mux.HandleFunc("POST /oauth/consent", a.consent)
	mux.HandleFunc("POST /account/login", a.loginStart)
	mux.HandleFunc("GET /account/callback", a.loginCallback)
	mux.HandleFunc("GET /account/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		io.WriteString(w, consentCSS)
	})
	mux.HandleFunc("POST /bind/key", a.bindKey)
	mux.HandleFunc("POST /account/logout", a.logout)
	mux.HandleFunc("POST /account/unlink", a.unlink)
	mux.HandleFunc("POST /account/delete", a.deleteAccount)
	mux.HandleFunc("/mcp", a.mcp)
	return a.boundary(mux)
}
func (a *App) boundary(next http.Handler) http.Handler {
	formOrigins := "'self' " + a.login.origin
	for _, redirects := range a.cfg.Clients {
		for _, raw := range redirects {
			u, _ := url.Parse(raw)
			formOrigins += " " + u.Scheme + "://" + u.Host
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action "+formOrigins+"; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		if r.Method == "GET" || r.Method == "HEAD" {
			if r.URL.Path == "/healthz" {
				writeJSON(w, 200, map[string]bool{"ok": true})
				return
			}
			if r.URL.Path == "/readyz" {
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				if a.store.Ready(ctx) != nil {
					writeJSON(w, 503, map[string]bool{"ready": false})
					return
				}
				writeJSON(w, 200, map[string]bool{"ready": true})
				return
			}
		}
		expected, _ := url.Parse(a.cfg.Issuer)
		if !strings.EqualFold(r.Host, expected.Host) {
			http.Error(w, "untrusted host", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		id := randomToken()
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		r = r.WithContext(ctx)
		for _, b := range []struct {
			key   string
			limit int
		}{{"http:global", 1200}, {"http:ip:" + digest(a.clientIP(r)), 180}} {
			ok, e := a.store.Allow(ctx, b.key, b.limit, time.Minute)
			if e != nil {
				http.Error(w, "temporarily unavailable", 503)
				return
			}
			if !ok {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "rate limit exceeded", 429)
				return
			}
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		a.logger.Info("request completed", "request_id", id, "route", safeRoute(r.URL.Path), "duration_ms", time.Since(start).Milliseconds())
	})
}

type requestIDKey struct{}

func safeRoute(path string) string {
	switch path {
	case "/mcp", "/oauth/authorize", "/oauth/token", "/oauth/revoke", "/oauth/consent", "/bind/key", "/account/login", "/account/callback", "/account/logout", "/account/unlink", "/account/delete":
		return path
	}
	return "other"
}
func (a *App) clientIP(r *http.Request) string {
	h, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return "unknown"
	}
	peer, e := netip.ParseAddr(h)
	if e != nil {
		return "unknown"
	}
	trusted := func(ip netip.Addr) bool {
		for _, p := range a.cfg.TrustedProxies {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer.String()
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(chain) > 16 {
		return peer.String()
	}
	for i := len(chain) - 1; i >= 0; i-- {
		ip, e := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if e != nil {
			return peer.String()
		}
		peer = ip
		if !trusted(ip) {
			break
		}
	}
	return peer.String()
}
func (a *App) mcp(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != a.cfg.Issuer && origin != "https://chatgpt.com" {
		http.Error(w, "untrusted origin", 403)
		return
	}
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.WriteHeader(204)
		return
	}
	p, e := a.authenticate(r.Context(), r.Header.Get("Authorization"))
	if e != nil {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+a.cfg.Issuer+`/.well-known/oauth-protected-resource", scope="`+scope+`"`)
		http.Error(w, "unauthorized", 401)
		return
	}
	_, account, _ := a.store.resolveCredential(r.Context(), p.UserID)
	requestID, _ := r.Context().Value(requestIDKey{}).(string)
	policy := &requestPolicy{a, p, account, requestID}
	mcpserver.NewPublicHandler(policy.runner(), policy, a.cache, a.logger).ServeHTTP(w, r)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func Run(ctx context.Context, c Config, out io.Writer) error {
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	s, e := Open(initCtx, c.DatabaseURL, c.Key)
	cancel()
	if e != nil {
		return e
	}
	defer s.Close()
	logger := slog.New(slog.NewJSONHandler(out, nil))
	initCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	a, e := New(initCtx, c, s, logger)
	cancel()
	if e != nil {
		return e
	}
	server := &http.Server{Addr: c.Listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 16 * time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	janitorCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.janitor(janitorCtx)
	select {
	case e := <-errs:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		e := server.Shutdown(shutdown)
		if e != nil {
			_ = server.Close()
		}
		return e
	}
}
func (a *App) janitor(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 10*time.Second)
		if e := a.store.Cleanup(work); e != nil {
			a.logger.Error("database cleanup failed")
		}
		cancel()
		if e := a.cache.Cleanup(); e != nil {
			a.logger.Error("media cleanup failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (a *App) deleteAccount(w http.ResponseWriter, r *http.Request) {
	f, ok := a.formFlow(w, r, true)
	if !ok {
		return
	}
	if r.PostForm.Get("confirm") != "delete" {
		http.Error(w, "confirmation required", 400)
		return
	}
	tx, e := a.store.DB.BeginTx(r.Context(), nil)
	if e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(r.Context(), `DELETE FROM audit_events WHERE user_id=$1`, f.UserID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if _, e = tx.ExecContext(r.Context(), `DELETE FROM app_users WHERE id=$1`, f.UserID); e != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	if tx.Commit() != nil {
		http.Error(w, "unavailable", 503)
		return
	}
	setCookie(w, sessionCookie, "", -1)
	writeJSON(w, 200, map[string]bool{"deleted": true})
}
