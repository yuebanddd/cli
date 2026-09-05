package chatgptapp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultListenAddress = "127.0.0.1:8787"
	DefaultMCPPath       = "/mcp"
)

type ServeOptions struct {
	ListenAddress string
	MCPPath       string
	AuthToken     string
}

func Serve(ctx context.Context, opts ServeOptions, runner *common.Runner, stdout, stderr io.Writer) error {
	if runner == nil || runner.Client == nil || runner.Auth == nil {
		return fmt.Errorf("ChatGPT App runner is incomplete")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	listenAddress := strings.TrimSpace(opts.ListenAddress)
	if listenAddress == "" {
		listenAddress = DefaultListenAddress
	}
	mcpPath, err := normalizeMCPPath(opts.MCPPath)
	if err != nil {
		return err
	}

	if _, err := runner.Auth.ResolveAccessKey(ctx); err != nil {
		return fmt.Errorf("Xiaoyunque login is required; run `pippit-tool-cli login` or set XYQ_ACCESS_KEY: %w", err)
	}

	service := NewService(runner)
	mcpServer := NewMCPServer(service)
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless:      true,
			JSONResponse:   true,
			SessionTimeout: 30 * time.Minute,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(mcpPath, bearerAuth(strings.TrimSpace(opts.AuthToken), http.MaxBytesHandler(mcpHandler, 2*1024*1024)))
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(`{"status":"ok"}`))
		}
	})

	httpServer := &http.Server{
		Addr:              listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	runContext, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errorChannel := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "Pippit ChatGPT App MCP server listening on http://%s%s\n", listenAddress, mcpPath)
		fmt.Fprintln(stdout, "Health check: /healthz")
		if strings.TrimSpace(opts.AuthToken) == "" {
			fmt.Fprintln(stderr, "Warning: MCP endpoint authentication is disabled; do not expose it publicly without access controls.")
		}
		errorChannel <- httpServer.ListenAndServe()
	}()

	select {
	case <-runContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down ChatGPT App server: %w", err)
		}
		return nil
	case err := <-errorChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve ChatGPT App MCP endpoint: %w", err)
	}
}

func NewMCPServer(service *Service) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "pippit-chatgpt-app",
			Version: version.Current(),
		},
		&mcp.ServerOptions{
			Instructions: "Use create_video only after the user explicitly asks to create a Xiaoyunque/Pippit video. When the user refers to an image or other media from the current ChatGPT conversation, pass it through the files parameter. Creating or revising a video uploads the selected files to Xiaoyunque and may consume the user's credits. Use get_video_status to read progress and results.",
		},
	)

	mcp.AddTool(server, createVideoTool(), func(ctx context.Context, _ *mcp.CallToolRequest, input CreateVideoInput) (*mcp.CallToolResult, VideoRunOutput, error) {
		output, err := service.CreateVideo(ctx, input)
		if err != nil {
			return nil, VideoRunOutput{}, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"Xiaoyunque video job submitted. thread_id=%s, run_id=%s%s",
				output.ThreadID,
				output.RunID,
				optionalLink(output.WebThreadLink),
			)}},
		}, *output, nil
	})

	mcp.AddTool(server, continueVideoTool(), func(ctx context.Context, _ *mcp.CallToolRequest, input ContinueVideoInput) (*mcp.CallToolResult, VideoRunOutput, error) {
		output, err := service.ContinueVideo(ctx, input)
		if err != nil {
			return nil, VideoRunOutput{}, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"Xiaoyunque revision submitted. thread_id=%s, run_id=%s%s",
				output.ThreadID,
				output.RunID,
				optionalLink(output.WebThreadLink),
			)}},
		}, *output, nil
	})

	mcp.AddTool(server, getVideoStatusTool(), func(ctx context.Context, _ *mcp.CallToolRequest, input GetVideoStatusInput) (*mcp.CallToolResult, VideoStatusOutput, error) {
		output, err := service.GetVideoStatus(ctx, input)
		if err != nil {
			return nil, VideoStatusOutput{}, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: output.ReadableText}},
		}, *output, nil
	})

	return server
}

func createVideoTool() *mcp.Tool {
	return &mcp.Tool{
		Name:         "create_video",
		Title:        "Create Xiaoyunque video",
		Description:  "Create a new Xiaoyunque/Pippit video job from an explicitly approved prompt, optionally using images, videos, or audio from the current ChatGPT conversation as reference material. This uploads the selected files to Xiaoyunque and may consume Xiaoyunque credits.",
		InputSchema:  videoInputSchema(false),
		OutputSchema: videoRunOutputSchema(),
		Annotations:  writeToolAnnotations("Create Xiaoyunque video"),
		Meta: mcp.Meta{
			"openai/fileParams":              []string{"files"},
			"openai/toolInvocation/invoking": "正在提交小云雀视频任务…",
			"openai/toolInvocation/invoked":  "小云雀视频任务已提交",
		},
	}
}

func continueVideoTool() *mcp.Tool {
	return &mcp.Tool{
		Name:         "continue_video",
		Title:        "Revise Xiaoyunque video",
		Description:  "Continue an existing Xiaoyunque/Pippit creative thread with revision instructions, optionally adding images, videos, or audio from the current ChatGPT conversation. This uploads selected files and may consume Xiaoyunque credits.",
		InputSchema:  videoInputSchema(true),
		OutputSchema: videoRunOutputSchema(),
		Annotations:  writeToolAnnotations("Revise Xiaoyunque video"),
		Meta: mcp.Meta{
			"openai/fileParams":              []string{"files"},
			"openai/toolInvocation/invoking": "正在向小云雀提交修改…",
			"openai/toolInvocation/invoked":  "小云雀修改任务已提交",
		},
	}
}

func getVideoStatusTool() *mcp.Tool {
	openWorld := true
	destructive := false
	return &mcp.Tool{
		Name:        "get_video_status",
		Title:       "Get Xiaoyunque video status",
		Description: "Read progress, messages, and result information for a Xiaoyunque/Pippit video run using the thread_id and optional run_id returned by create_video or continue_video.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"thread_id": map[string]any{"type": "string", "minLength": 1},
				"run_id":    map[string]any{"type": "string", "minLength": 1},
			},
			"required": []string{"thread_id"},
		},
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"thread_id":     map[string]any{"type": "string"},
				"run_id":        map[string]any{"type": "string"},
				"readable_text": map[string]any{"type": "string"},
			},
			"required": []string{"thread_id", "run_id", "readable_text"},
		},
		Annotations: &mcp.ToolAnnotations{
			Title:           "Get Xiaoyunque video status",
			ReadOnlyHint:    true,
			DestructiveHint: &destructive,
			OpenWorldHint:   &openWorld,
		},
		Meta: mcp.Meta{
			"openai/toolInvocation/invoking": "正在查询小云雀生成进度…",
			"openai/toolInvocation/invoked":  "已获取小云雀生成进度",
		},
	}
}

func videoInputSchema(includeThreadID bool) map[string]any {
	properties := map[string]any{
		"prompt": map[string]any{
			"type":      "string",
			"minLength": 1,
		},
		"files": map[string]any{
			"type":     "array",
			"maxItems": maxChatGPTFiles,
			"items":    map[string]any{"$ref": "#/$defs/OpenAIFile"},
		},
	}
	required := []string{"prompt"}
	if includeThreadID {
		properties["thread_id"] = map[string]any{
			"type":      "string",
			"minLength": 1,
		}
		required = append([]string{"thread_id"}, required...)
	}
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"$defs": map[string]any{
			"OpenAIFile": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"download_url": map[string]any{"type": "string"},
					"file_id":      map[string]any{"type": "string"},
					"mime_type":    map[string]any{"type": "string"},
					"file_name":    map[string]any{"type": "string"},
				},
				"required": []string{"download_url", "file_id"},
			},
		},
		"properties": properties,
		"required":   required,
	}
}

func videoRunOutputSchema() map[string]any {
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"thread_id":          map[string]any{"type": "string"},
			"run_id":             map[string]any{"type": "string"},
			"web_thread_link":    map[string]any{"type": "string"},
			"uploaded_asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"thread_id", "run_id", "web_thread_link", "uploaded_asset_ids"},
	}
}

func writeToolAnnotations(title string) *mcp.ToolAnnotations {
	openWorld := true
	destructive := false
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
	}
}

func optionalLink(link string) string {
	if strings.TrimSpace(link) == "" {
		return ""
	}
	return ", web_thread_link=" + link
}

func normalizeMCPPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultMCPPath
	}
	if !strings.HasPrefix(path, "/") || path == "/" {
		return "", fmt.Errorf("MCP path must start with '/' and must not be the root path")
	}
	return strings.TrimSuffix(path, "/"), nil
}

func bearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		expected := "Bearer " + token
		provided := request.Header.Get("Authorization")
		if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="pippit-chatgpt-app"`)
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
