package mcpserver

import (
	"path/filepath"
	"testing"
)

func TestOptionsNormalizeLoopback(t *testing.T) {
	opts := Options{
		Listen:              "127.0.0.1:8787",
		Path:                "mcp/",
		HealthPath:          "healthz/",
		OutputDir:           t.TempDir(),
		MaxFileBytes:        1024,
		MaxRequestBodyBytes: 2048,
	}
	normalized, err := opts.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if normalized.Path != "/mcp" || normalized.HealthPath != "/healthz" {
		t.Fatalf("normalized paths = %q, %q", normalized.Path, normalized.HealthPath)
	}
	if !matchHost("localhost:8787", normalized.AllowedHosts) {
		t.Fatalf("loopback defaults did not allow localhost: %#v", normalized.AllowedHosts)
	}
}

func TestOptionsNormalizeRemoteRequiresProtection(t *testing.T) {
	opts := Options{
		Listen:              "0.0.0.0:8787",
		Path:                "/mcp",
		HealthPath:          "/healthz",
		OutputDir:           t.TempDir(),
		MaxFileBytes:        1024,
		MaxRequestBodyBytes: 2048,
	}
	if _, err := opts.Normalize(); err == nil {
		t.Fatal("Normalize() succeeded for an unprotected remote listener")
	}
	opts.AllowedHosts = []string{"mcp.example.com"}
	opts.AuthToken = "secret"
	if _, err := opts.Normalize(); err != nil {
		t.Fatalf("Normalize() protected listener error = %v", err)
	}
}

func TestOutputPathCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	service := &service{options: Options{OutputDir: root}}
	if _, err := service.outputPath("../escape", "result.mp4"); err == nil {
		t.Fatal("outputPath accepted path traversal")
	}
	path, err := service.outputPath("jobs/one", "result.mp4")
	if err != nil {
		t.Fatalf("outputPath error = %v", err)
	}
	want := filepath.Join(root, "jobs", "one", "result.mp4")
	if path != want {
		t.Fatalf("outputPath = %q, want %q", path, want)
	}
}

func TestHostOriginAndBearerMatching(t *testing.T) {
	if !matchHost("mcp.example.com:443", []string{"*.example.com"}) {
		t.Fatal("wildcard host did not match")
	}
	if matchHost("example.com", []string{"*.example.com"}) {
		t.Fatal("wildcard host matched apex")
	}
	if !matchOrigin("https://chatgpt.com", "mcp.example.com", []string{"https://chatgpt.com"}) {
		t.Fatal("configured ChatGPT origin did not match")
	}
	if matchOrigin("https://evil.example", "mcp.example.com", []string{"https://chatgpt.com"}) {
		t.Fatal("untrusted origin matched")
	}
	if !validBearerToken("Bearer secret", "secret") || validBearerToken("Bearer wrong", "secret") {
		t.Fatal("bearer token validation mismatch")
	}
}
