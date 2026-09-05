package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PublicPolicy is the durable authorization/ownership/idempotency boundary.
// Implementations MUST bind to a server-verified OAuth principal, not tool args.
type PublicPolicy interface {
	Execute(context.Context, string, bool, []byte, func(context.Context) ([]byte, error)) ([]byte, error)
}

// NewPublicHandler shares the local tool implementations with a request-scoped
// runner. A public handler is constructed only AFTER OAuth authentication.
func NewPublicHandler(runner *common.Runner, policy PublicPolicy, cache *MediaCache, logger *slog.Logger) http.Handler {
	if runner == nil || runner.Auth == nil || runner.Client == nil || policy == nil || cache == nil {
		panic("public MCP dependencies required")
	}
	opts := DefaultOptions()
	opts.AllowPrivateFileURLs = false
	opts.AuthToken = ""
	opts.OutputDir = ""
	opts.MaxFileBytes = cache.MaxFileBytes
	s := newService(runner, opts)
	s.public = policy
	s.downloader.cache = cache
	s.downloader.fakeIP = cache.AllowChatGPTFakeIP
	s.downloader.publicInputs = true
	server := s.newProtocolServer(logger)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger})
}
func addTool[In, Out any](s *service, server *mcp.Server, t *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	if s.public == nil {
		mcp.AddTool(server, t, handler)
		return
	}
	if t.Name == "pippit_download_result" {
		return
	}
	t.Meta["securitySchemes"] = []map[string]any{{"type": "oauth2", "scopes": []string{"xiaoyunque:tools"}}}
	if t.Name == "pippit_auth_status" {
		return
	} // dedicated public status omits identifiers
	if !t.Annotations.ReadOnlyHint {
		t.Description += " Submit once; poll many. idempotency_key is required in public mode: reuse the SAME key and arguments for retries; a different key means a new operation that may charge credits."
	}
	mcp.AddTool(server, t, func(ctx context.Context, r *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var output Out
		b, e := json.Marshal(in)
		if e != nil {
			return nil, output, e
		}
		raw, e := s.public.Execute(ctx, t.Name, t.Annotations.ReadOnlyHint, b, func(ctx context.Context) ([]byte, error) {
			result, out, err := handler(ctx, r, in)
			if err != nil {
				return nil, err
			}
			if result != nil && result.IsError {
				return nil, fmt.Errorf("upstream_error")
			}
			return json.Marshal(out)
		})
		if e != nil {
			if strings.Contains(e.Error(), "reauthorization_required") {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "reauthorization_required: 请重新连接小云雀。"}}, Meta: mcp.Meta{"mcp/www_authenticate": []string{`Bearer error="invalid_token", error_description="Reconnect Xiaoyunque", scope="xiaoyunque:tools"`}}}, output, nil
			}
			return nil, output, e
		}
		e = json.Unmarshal(raw, &output)
		return nil, output, e
	})
}

type accountStatusOutput struct {
	Connected           bool `json:"connected"`
	XiaoyunqueConnected bool `json:"xiaoyunque_connected"`
}

func (s *service) registerAccountStatus(server *mcp.Server) {
	t := toolDefinition("pippit_account_status", "检查小云雀连接", "检查当前 OAuth 用户自己的小云雀连接状态，不返回账号标识或凭据。", true, false, true, false, nil, "正在检查连接…", "已检查连接")
	t.Meta["securitySchemes"] = []map[string]any{{"type": "oauth2", "scopes": []string{"xiaoyunque:tools"}}}
	mcp.AddTool(server, t, func(ctx context.Context, r *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, accountStatusOutput, error) {
		var output accountStatusOutput
		b, e := s.public.Execute(ctx, t.Name, true, []byte(`{}`), func(ctx context.Context) ([]byte, error) {
			_, e := s.runner.Auth.ResolveAccessKey(ctx)
			return json.Marshal(accountStatusOutput{true, e == nil})
		})
		if e != nil {
			return nil, output, e
		}
		e = json.Unmarshal(b, &output)
		return nil, output, e
	})
	t.Meta["securitySchemes"] = []map[string]any{{"type": "oauth2", "scopes": []string{"xiaoyunque:tools"}}}
}
