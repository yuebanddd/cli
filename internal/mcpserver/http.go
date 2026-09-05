package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run starts the native Streamable HTTP MCP server and blocks until the
// context is cancelled or the listener fails.
func Run(ctx context.Context, rawOptions Options, runner *common.Runner, output io.Writer) error {
	options, err := rawOptions.Normalize()
	if err != nil {
		return err
	}
	if runner == nil || runner.Client == nil || runner.Auth == nil {
		return fmt.Errorf("MCP server requires a fully initialized CLI runner")
	}
	if _, err := runner.Auth.ResolveAccessKey(ctx); err != nil {
		return fmt.Errorf("MCP server cannot resolve Xiaoyunque credentials; run pippit-tool-cli login or set XYQ_ACCESS_KEY: %w", err)
	}
	if err := os.MkdirAll(options.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create MCP output directory: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	service := newService(runner, options)
	protocolServer := service.newProtocolServer(logger)
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return protocolServer },
		&mcp.StreamableHTTPOptions{
			Stateless:      true,
			JSONResponse:   true,
			Logger:         logger,
			SessionTimeout: options.IdleTimeout,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(options.Path, secureMCPHandler(streamable, options))
	mux.HandleFunc(options.HealthPath, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if request.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"service":"pippit-cli-mcp"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprintf(w, `{"service":"pippit-cli-mcp","mcp_path":%q,"health_path":%q}`, options.Path, options.HealthPath)
	})

	httpServer := &http.Server{
		Addr:              options.Listen,
		Handler:           mux,
		ReadHeaderTimeout: options.ReadHeaderTimeout,
		IdleTimeout:       options.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("Pippit MCP server started",
			"listen", options.Listen,
			"path", options.Path,
			"health_path", options.HealthPath,
			"output_dir", options.OutputDir,
		)
		serveErrors <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve MCP HTTP endpoint: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), options.GracefulShutdownDelay)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down MCP HTTP server: %w", err)
		}
		select {
		case err := <-serveErrors:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve MCP HTTP endpoint: %w", err)
			}
		case <-time.After(options.GracefulShutdownDelay):
			return fmt.Errorf("MCP HTTP server did not stop after shutdown")
		}
		return nil
	}
}

func secureMCPHandler(next http.Handler, options Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")

		if !matchHost(request.Host, options.AllowedHosts) {
			http.Error(w, "untrusted Host header", http.StatusForbidden)
			return
		}
		origin := strings.TrimSpace(request.Header.Get("Origin"))
		if origin != "" && !matchOrigin(origin, request.Host, options.AllowedOrigins) {
			http.Error(w, "untrusted Origin header", http.StatusForbidden)
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Last-Event-ID, Mcp-Session-Id, Mcp-Protocol-Version")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
		if request.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if options.AuthToken != "" && !validBearerToken(request.Header.Get("Authorization"), options.AuthToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pippit-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if request.Method == http.MethodPost {
			request.Body = http.MaxBytesReader(w, request.Body, options.MaxRequestBodyBytes)
		}
		next.ServeHTTP(w, request)
	})
}

func validBearerToken(header, expected string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	actual := strings.TrimSpace(header[len(prefix):])
	if len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func matchHost(rawHost string, patterns []string) bool {
	hostname := hostnameOnly(rawHost)
	if hostname == "" {
		return false
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "*" {
			return true
		}
		patternHost := hostnameOnly(pattern)
		if patternHost == "" {
			patternHost = strings.Trim(strings.ToLower(pattern), "[]")
		}
		if matchHostnamePattern(hostname, patternHost) {
			return true
		}
	}
	return false
}

func matchOrigin(rawOrigin, requestHost string, patterns []string) bool {
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.Scheme == "" || origin.Hostname() == "" || origin.User != nil {
		return false
	}
	originScheme := strings.ToLower(origin.Scheme)
	originHost := strings.ToLower(origin.Hostname())
	originPort := origin.Port()

	if len(patterns) == 0 {
		requestHostname := hostnameOnly(requestHost)
		if originHost == requestHostname {
			return true
		}
		return isLoopbackHostname(originHost) && isLoopbackHostname(requestHostname)
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "*" {
			return true
		}
		parsedPattern, err := url.Parse(pattern)
		if err == nil && parsedPattern.Scheme != "" && parsedPattern.Hostname() != "" {
			if !strings.EqualFold(parsedPattern.Scheme, originScheme) {
				continue
			}
			if parsedPattern.Port() != "" && parsedPattern.Port() != originPort {
				continue
			}
			if matchHostnamePattern(originHost, parsedPattern.Hostname()) {
				return true
			}
			continue
		}
		if matchHostnamePattern(originHost, hostnameOnly(pattern)) {
			return true
		}
	}
	return false
}

func hostnameOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err == nil {
			return strings.ToLower(parsed.Hostname())
		}
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return strings.ToLower(strings.Trim(host, "[]"))
	}
	return strings.ToLower(strings.Trim(raw, "[]"))
}

func matchHostnamePattern(hostname, pattern string) bool {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	pattern = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
	if hostname == "" || pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(hostname, suffix) && hostname != strings.TrimPrefix(suffix, ".")
	}
	return hostname == pattern
}

func isLoopbackHostname(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
