package mcpserver

import (
	"log/slog"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const nestAgentName = "pippit_nest_agent"

// ToolNames is the stable MCP surface exposed by this server. Authentication,
// logout, and self-update remain CLI control-plane operations and are not
// callable by a conversational model.
var ToolNames = []string{
	"pippit_auth_status",
	"pippit_upload_media",
	"pippit_nest_submit",
	"pippit_generate_image",
	"pippit_generate_video",
	"pippit_query_result",
	"pippit_get_thread",
	"pippit_list_thread_files",
	"pippit_download_result",
	"pippit_short_drama_submit",
	"pippit_short_drama_upload",
	"pippit_video_super_resolution",
	"pippit_erase_video_subtitle",
	"pippit_canvas_create",
	"pippit_canvas_allocate",
	"pippit_canvas_get",
	"pippit_canvas_apply",
	"pippit_canvas_upload",
	"pippit_canvas_command_list",
	"pippit_canvas_command_describe",
	"pippit_canvas_command_run",
}

type service struct {
	runner     *common.Runner
	options    Options
	downloader *mediaDownloader
}

func newService(runner *common.Runner, options Options) *service {
	return &service{
		runner:     runner,
		options:    options,
		downloader: newMediaDownloader(options.MaxFileBytes, options.AllowPrivateFileURLs),
	}
}

func (s *service) newProtocolServer(logger *slog.Logger) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "pippit-cli-mcp", Version: version.Current()},
		&mcp.ServerOptions{
			Logger: logger,
			Instructions: "Use Pippit/Xiaoyunque tools for video, image, short-drama, editing, and Canvas workflows. " +
				"Prefer ChatGPT's native image generation for ordinary image creation, then pass the approved generated image through a tool file parameter when the user wants Xiaoyunque to animate or edit it. " +
				"ChatGPT-generated or user-uploaded files may be passed through file parameters. " +
				"Generation and editing tools can consume Xiaoyunque credits; call them only after the user has requested or approved the operation. " +
				"Use get/query tools to inspect asynchronous results and reuse thread_id for revisions.",
		},
	)
	s.registerCommonTools(server)
	s.registerGenerationTools(server)
	s.registerShortDramaTools(server)
	s.registerVideoTools(server)
	s.registerCanvasTools(server)
	return server
}

func toolDefinition(name, title, description string, readOnly, destructive, idempotent, openWorld bool, fileParams []string, invoking, invoked string) *mcp.Tool {
	meta := mcp.Meta{}
	if len(fileParams) > 0 {
		meta["openai/fileParams"] = append([]string(nil), fileParams...)
	}
	if invoking != "" {
		meta["openai/toolInvocation/invoking"] = invoking
	}
	if invoked != "" {
		meta["openai/toolInvocation/invoked"] = invoked
	}
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Meta:        meta,
		Annotations: &mcp.ToolAnnotations{
			Title:           title,
			ReadOnlyHint:    readOnly,
			DestructiveHint: boolPointer(destructive),
			IdempotentHint:  idempotent,
			OpenWorldHint:   boolPointer(openWorld),
		},
	}
}

func boolPointer(value bool) *bool {
	return &value
}
