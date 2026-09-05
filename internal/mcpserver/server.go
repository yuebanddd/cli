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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultListenAddress = "127.0.0.1:8787"
	DefaultMCPPath       = "/mcp"
	BearerTokenEnv       = "PIPPIT_MCP_BEARER_TOKEN"
)

// ServeOptions configures the MCP Streamable HTTP endpoint.
type ServeOptions struct {
	ListenAddress      string
	MCPPath            string
	Workspace          string
	BearerToken        string
	AllowedOrigin      string
	AllowInsecurePublic bool
}

// DefaultWorkspace returns the directory used for server-side output files.
func DefaultWorkspace() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".", ".pippit-mcp")
	}
	return filepath.Join(home, ".pippit_tool_cli", "mcp")
}

// Serve starts a Streamable HTTP MCP server and blocks until ctx is cancelled
// or the HTTP listener exits.
func Serve(ctx context.Context, opts ServeOptions, runner *common.Runner, stdout, stderr io.Writer) error {
	if runner == nil || runner.Client == nil || runner.Auth == nil {
		return fmt.Errorf("MCP server requires a fully configured Pippit runner")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if strings.TrimSpace(opts.ListenAddress) == "" {
		opts.ListenAddress = DefaultListenAddress
	}
	path, err := normalizeMCPPath(opts.MCPPath)
	if err != nil {
		return err
	}
	opts.MCPPath = path
	if strings.TrimSpace(opts.Workspace) == "" {
		opts.Workspace = DefaultWorkspace()
	}
	workspace, err := common.ExpandPath(opts.Workspace)
	if err != nil {
		return fmt.Errorf("resolve MCP workspace: %w", err)
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return fmt.Errorf("resolve MCP workspace: %w", err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fmt.Errorf("create MCP workspace: %w", err)
	}
	opts.Workspace = workspace
	if strings.TrimSpace(opts.AllowedOrigin) == "" {
		opts.AllowedOrigin = "*"
	}

	listener, err := net.Listen("tcp", opts.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.ListenAddress, err)
	}
	defer listener.Close()
	if strings.TrimSpace(opts.BearerToken) == "" && !isLoopbackListener(listener.Addr()) && !opts.AllowInsecurePublic {
		return fmt.Errorf("refusing to expose an unauthenticated MCP server on %s; set %s or pass --allow-insecure-public", listener.Addr(), BearerTokenEnv)
	}

	mcpServer := NewServer(runner, workspace)
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless:      true,
			JSONResponse:   true,
			SessionTimeout: 30 * time.Minute,
			Logger: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			})),
		},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"pippit-tool-cli-mcp","version":%q,"mcp_path":%q}`, version.Current(), opts.MCPPath)
	})
	mux.Handle(opts.MCPPath, mcpHTTPMiddleware(streamable, opts))

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()
	_, _ = fmt.Fprintf(stdout, "Pippit MCP server listening on http://%s%s\n", listener.Addr().String(), opts.MCPPath)
	_, _ = fmt.Fprintf(stdout, "Workspace: %s\n", workspace)
	if strings.TrimSpace(opts.BearerToken) == "" {
		_, _ = fmt.Fprintln(stdout, "MCP endpoint authentication: disabled")
	} else {
		_, _ = fmt.Fprintln(stdout, "MCP endpoint authentication: bearer token enabled")
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		serveErr := <-errCh
		if shutdownErr != nil {
			return fmt.Errorf("shutdown MCP server: %w", shutdownErr)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve MCP endpoint: %w", serveErr)
		}
		return nil
	case serveErr := <-errCh:
		if serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve MCP endpoint: %w", serveErr)
	}
}

func normalizeMCPPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultMCPPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	if path == "" || path == "/" {
		return "", fmt.Errorf("MCP path must not be the server root")
	}
	if strings.Contains(path, "?") || strings.Contains(path, "#") {
		return "", fmt.Errorf("MCP path must not contain a query or fragment")
	}
	return path, nil
}

func isLoopbackListener(addr net.Addr) bool {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	return tcpAddr.IP != nil && tcpAddr.IP.IsLoopback()
}

func mcpHTTPMiddleware(next http.Handler, opts ServeOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origin := strings.TrimSpace(opts.AllowedOrigin)
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-Id")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if token := strings.TrimSpace(opts.BearerToken); token != "" {
			supplied := strings.TrimSpace(req.Header.Get("Authorization"))
			prefix := "Bearer "
			if len(supplied) <= len(prefix) || !strings.EqualFold(supplied[:len(prefix)], prefix) ||
				subtle.ConstantTimeCompare([]byte(strings.TrimSpace(supplied[len(prefix):])), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pippit-mcp"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		if req.Body != nil {
			req.Body = http.MaxBytesReader(w, req.Body, 4<<20)
		}
		next.ServeHTTP(w, req)
	})
}
