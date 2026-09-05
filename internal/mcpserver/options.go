package mcpserver

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddress = "127.0.0.1:8787"
	defaultMCPPath       = "/mcp"
	defaultHealthPath    = "/healthz"
	defaultMaxFileBytes  = int64(200 << 20)
	defaultMaxBodyBytes  = int64(8 << 20)
)

// Options configures the native Streamable HTTP MCP server.
type Options struct {
	Listen                string
	Path                  string
	HealthPath            string
	AuthToken             string
	AllowedHosts          []string
	AllowedOrigins        []string
	OutputDir             string
	CLICommand            string
	AllowPrivateFileURLs  bool
	MaxFileBytes          int64
	MaxRequestBodyBytes   int64
	ReadHeaderTimeout     time.Duration
	IdleTimeout           time.Duration
	GracefulShutdownDelay time.Duration
}

// DefaultOptions returns secure defaults for a local private MCP server.
func DefaultOptions() Options {
	return Options{
		Listen:                envOr("PIPPIT_MCP_LISTEN", defaultListenAddress),
		Path:                  envOr("PIPPIT_MCP_PATH", defaultMCPPath),
		HealthPath:            envOr("PIPPIT_MCP_HEALTH_PATH", defaultHealthPath),
		AuthToken:             strings.TrimSpace(os.Getenv("PIPPIT_MCP_AUTH_TOKEN")),
		AllowedHosts:          splitCSV(os.Getenv("PIPPIT_MCP_ALLOWED_HOSTS")),
		AllowedOrigins:        splitCSV(os.Getenv("PIPPIT_MCP_ALLOWED_ORIGINS")),
		OutputDir:             envOr("PIPPIT_MCP_OUTPUT_DIR", "~/.pippit_tool_cli/mcp-output"),
		CLICommand:            envOr("PIPPIT_MCP_CLI_COMMAND", "pippit-tool-cli"),
		AllowPrivateFileURLs:  envBool("PIPPIT_MCP_ALLOW_PRIVATE_FILE_URLS", false),
		MaxFileBytes:          envInt64("PIPPIT_MCP_MAX_FILE_BYTES", defaultMaxFileBytes),
		MaxRequestBodyBytes:   envInt64("PIPPIT_MCP_MAX_REQUEST_BODY_BYTES", defaultMaxBodyBytes),
		ReadHeaderTimeout:     10 * time.Second,
		IdleTimeout:           2 * time.Minute,
		GracefulShutdownDelay: 10 * time.Second,
	}
}

// Normalize validates options and returns a copy with canonical paths and host defaults.
func (opts Options) Normalize() (Options, error) {
	opts.Listen = strings.TrimSpace(opts.Listen)
	if opts.Listen == "" {
		opts.Listen = defaultListenAddress
	}
	host, _, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return Options{}, fmt.Errorf("invalid MCP listen address %q: %w", opts.Listen, err)
	}

	opts.Path, err = normalizeHTTPPath(opts.Path, defaultMCPPath)
	if err != nil {
		return Options{}, fmt.Errorf("invalid MCP path: %w", err)
	}
	opts.HealthPath, err = normalizeHTTPPath(opts.HealthPath, defaultHealthPath)
	if err != nil {
		return Options{}, fmt.Errorf("invalid health path: %w", err)
	}
	if opts.Path == opts.HealthPath {
		return Options{}, fmt.Errorf("MCP path and health path must be different")
	}

	opts.OutputDir, err = expandHome(strings.TrimSpace(opts.OutputDir))
	if err != nil {
		return Options{}, fmt.Errorf("resolve MCP output directory: %w", err)
	}
	if opts.OutputDir == "" {
		return Options{}, fmt.Errorf("MCP output directory must not be empty")
	}
	opts.OutputDir, err = filepath.Abs(opts.OutputDir)
	if err != nil {
		return Options{}, fmt.Errorf("resolve absolute MCP output directory: %w", err)
	}

	opts.CLICommand = strings.TrimSpace(opts.CLICommand)
	if opts.CLICommand == "" {
		opts.CLICommand = "pippit-tool-cli"
	}
	opts.AuthToken = strings.TrimSpace(opts.AuthToken)
	opts.AllowedHosts = normalizePatterns(opts.AllowedHosts)
	opts.AllowedOrigins = normalizePatterns(opts.AllowedOrigins)

	if isLoopbackBindHost(host) {
		opts.AllowedHosts = appendUnique(opts.AllowedHosts, "localhost", "127.0.0.1", "::1")
		if host != "" {
			opts.AllowedHosts = appendUnique(opts.AllowedHosts, host)
		}
	} else {
		if len(opts.AllowedHosts) == 0 {
			return Options{}, fmt.Errorf("non-loopback MCP listeners require --allowed-host or PIPPIT_MCP_ALLOWED_HOSTS")
		}
		if opts.AuthToken == "" {
			return Options{}, fmt.Errorf("non-loopback MCP listeners require --auth-token or PIPPIT_MCP_AUTH_TOKEN")
		}
	}

	if opts.MaxFileBytes <= 0 || opts.MaxFileBytes > defaultMaxFileBytes {
		return Options{}, fmt.Errorf("max file size must be between 1 and %d bytes", defaultMaxFileBytes)
	}
	if opts.MaxRequestBodyBytes <= 0 {
		return Options{}, fmt.Errorf("max MCP request body size must be greater than zero")
	}
	if opts.ReadHeaderTimeout <= 0 {
		opts.ReadHeaderTimeout = 10 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 2 * time.Minute
	}
	if opts.GracefulShutdownDelay <= 0 {
		opts.GracefulShutdownDelay = 10 * time.Second
	}
	return opts, nil
}

func normalizeHTTPPath(raw, fallback string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fallback
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	if strings.ContainsAny(raw, "?#") {
		return "", fmt.Errorf("path %q must not contain query or fragment components", raw)
	}
	if len(raw) > 1 {
		raw = strings.TrimRight(raw, "/")
	}
	return raw, nil
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func normalizePatterns(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = appendUnique(result, value)
		}
	}
	return result
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[strings.ToLower(strings.TrimSpace(value))] = struct{}{}
	}
	for _, value := range additions {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, value)
	}
	return values
}

func isLoopbackBindHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
