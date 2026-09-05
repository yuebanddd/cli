package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	imagegen "github.com/Pippit-dev/pippit-cli/internal/generate_image"
	videogen "github.com/Pippit-dev/pippit-cli/internal/generate_video"
	videotool "github.com/Pippit-dev/pippit-cli/internal/video_tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type submitRunOutput struct {
	ThreadID        string   `json:"thread_id"`
	RunID           string   `json:"run_id"`
	WebThreadLink   string   `json:"web_thread_link,omitempty"`
	UploadedAssetIDs []string `json:"uploaded_asset_ids,omitempty"`
}

type uploadFileInput struct {
	Files []FileReference `json:"files" jsonschema:"Exactly one file from the current ChatGPT conversation"`
}

type uploadFileOutput struct {
	AssetID  string `json:"asset_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
}

type generateImageInput struct {
	Prompt             string          `json:"prompt" jsonschema:"Image generation or image editing instruction"`
	Model              string          `json:"model" jsonschema:"Pippit image model, for example seedream_5.0 or seedream_5.0_pro"`
	Ratio              string          `json:"ratio,omitempty" jsonschema:"Pippit ratio enum: 0, 2, 13, 3, 4, 5, or 6"`
	Resolution         string          `json:"resolution,omitempty" jsonschema:"Optional image resolution such as 1K, 2K, or 4K where supported"`
	GenerateImageCount *int            `json:"generate_image_count,omitempty" jsonschema:"Optional number of images to generate"`
	Files              []FileReference `json:"files,omitempty" jsonschema:"Optional reference images from the current ChatGPT conversation"`
}

type generateVideoInput struct {
	Prompt       string          `json:"prompt" jsonschema:"Video generation or editing instruction"`
	DurationSec  *int            `json:"duration_seconds,omitempty" jsonschema:"Requested video duration in seconds"`
	Ratio        string          `json:"ratio,omitempty" jsonschema:"Video aspect ratio such as 9:16, 16:9, 3:4, or 4:3"`
	Model        string          `json:"model,omitempty" jsonschema:"Optional Pippit video model"`
	Resolution   string          `json:"resolution,omitempty" jsonschema:"Optional video resolution such as 720p or 1080p"`
	GenerateType *int64          `json:"generate_type,omitempty" jsonschema:"Set to 1 for first-and-last-frame generation; attach exactly two images in first-frame, last-frame order"`
	Files        []FileReference `json:"files,omitempty" jsonschema:"Reference images, videos, or audio from the current ChatGPT conversation"`
}

type nestSubmitInput struct {
	Message  string          `json:"message" jsonschema:"Natural-language creative request for Xiaoyunque NestAgent"`
	ThreadID string          `json:"thread_id,omitempty" jsonschema:"Existing thread ID for follow-up or revision; omit to start a new thread"`
	AssetIDs []string        `json:"asset_ids,omitempty" jsonschema:"Existing Xiaoyunque asset IDs to attach"`
	Files    []FileReference `json:"files,omitempty" jsonschema:"Images, videos, audio, or documents from the current ChatGPT conversation"`
}

type queryResultInput struct {
	ThreadID      string `json:"thread_id" jsonschema:"Thread ID returned by a generation tool"`
	RunID         string `json:"run_id" jsonschema:"Run ID returned by a generation tool"`
	DownloadSubdir string `json:"download_subdir,omitempty" jsonschema:"Optional relative directory inside the MCP workspace for completed files"`
}

type videoSuperResolutionInput struct {
	OutputResolution string          `json:"output_resolution" jsonschema:"Target resolution: 720p, 1080p, 2k, or 4k"`
	ToolVersion      string          `json:"tool_version,omitempty" jsonschema:"Optional tool version: standard, professional_v1, or professional_v2"`
	Files            []FileReference `json:"files" jsonschema:"Exactly one video from the current ChatGPT conversation"`
}

type eraseVideoSubtitleInput struct {
	Files []FileReference `json:"files" jsonschema:"Exactly one video from the current ChatGPT conversation"`
}

func (r *toolRegistrar) registerMediaTools(server *mcp.Server) {
	mcp.AddTool(server, toolDefinition(
		"pippit_upload_file",
		"Upload file to Xiaoyunque",
		"Upload one ChatGPT conversation file to Xiaoyunque and return its durable asset_id for later tools.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input uploadFileInput) (*mcp.CallToolResult, uploadFileOutput, error) {
		files, cleanup, err := r.prepareFiles(ctx, input.Files)
		if err != nil {
			return nil, uploadFileOutput{}, err
		}
		defer cleanup()
		file, err := requireSingleFile(files, "")
		if err != nil {
			return nil, uploadFileOutput{}, err
		}
		result, err := common.UploadFile(ctx, common.UploadFileOptions{Path: file.Path, FileName: file.FileName}, r.runner)
		if err != nil {
			return nil, uploadFileOutput{}, err
		}
		return nil, uploadFileOutput{AssetID: result.AssetID, FileName: file.FileName, MIMEType: file.MIMEType}, nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_generate_image",
		"Generate image with Xiaoyunque",
		"Generate or edit an image with a Xiaoyunque image model. ChatGPT already has native image generation, so use this only when the user explicitly asks for Xiaoyunque/Pippit image generation. Attached files must be images.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input generateImageInput) (*mcp.CallToolResult, submitRunOutput, error) {
		files, cleanup, err := r.prepareFiles(ctx, input.Files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		defer cleanup()
		images, videos, audios, documents, err := splitPreparedFiles(files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		if len(videos)+len(audios)+len(documents) != 0 {
			return nil, submitRunOutput{}, fmt.Errorf("pippit_generate_image accepts image references only")
		}
		result, err := imagegen.Run(ctx, &imagegen.Options{
			Prompt:             input.Prompt,
			ImagePaths:         images,
			Model:              input.Model,
			Ratio:              input.Ratio,
			Resolution:         input.Resolution,
			GenerateImageCount: input.GenerateImageCount,
		}, r.runner)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		return nil, submitOutput(result.ThreadID, result.RunID, result.WebThreadLink, nil), nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_generate_video",
		"Generate video with Xiaoyunque",
		"Submit direct Xiaoyunque video generation. To animate a ChatGPT-generated image, attach that conversation image in files. Images are kept in the supplied order; for first/last frame mode attach exactly two images and set generate_type=1.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input generateVideoInput) (*mcp.CallToolResult, submitRunOutput, error) {
		files, cleanup, err := r.prepareFiles(ctx, input.Files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		defer cleanup()
		images, videos, audios, documents, err := splitPreparedFiles(files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		if len(documents) != 0 {
			return nil, submitRunOutput{}, fmt.Errorf("pippit_generate_video does not accept document files")
		}
		result, err := videogen.Run(ctx, &videogen.Options{
			Prompt:       input.Prompt,
			ImagePaths:   images,
			VideoPaths:   videos,
			AudioPaths:   audios,
			DurationSec:  input.DurationSec,
			Ratio:        input.Ratio,
			Model:        input.Model,
			Resolution:   input.Resolution,
			GenerateType: input.GenerateType,
		}, r.runner)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		return nil, submitOutput(result.ThreadID, result.RunID, result.WebThreadLink, nil), nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_nest_submit",
		"Create or revise with Xiaoyunque Agent",
		"Send a natural-language request and optional ChatGPT files to the general Xiaoyunque creative agent. Use an existing thread_id for revisions and follow-up instructions. This is suited to storyboards, ads, TVCs, MV creation, and mixed-reference workflows.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input nestSubmitInput) (*mcp.CallToolResult, submitRunOutput, error) {
		if strings.TrimSpace(input.Message) == "" {
			return nil, submitRunOutput{}, fmt.Errorf("message is required")
		}
		files, cleanup, err := r.prepareFiles(ctx, input.Files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		defer cleanup()
		uploaded, err := r.uploadPreparedFiles(ctx, files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		assetIDs := append([]string(nil), input.AssetIDs...)
		assetIDs = append(assetIDs, uploaded...)
		body := map[string]any{"message": strings.TrimSpace(input.Message)}
		if threadID := strings.TrimSpace(input.ThreadID); threadID != "" {
			body["thread_id"] = threadID
		}
		if len(assetIDs) > 0 {
			body["asset_ids"] = assetIDs
		}
		result, err := common.SubmitRun(ctx, "nest-submit", body, r.runner)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		return nil, submitOutput(result.ThreadID, result.RunID, result.WebThreadLink, uploaded), nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_query_result",
		"Query and download generation result",
		"Query one generation run. When complete, download returned videos/images into the MCP server workspace and return both source URLs and server-side paths. Repeated calls are safe.",
		true, false, true,
	), func(ctx context.Context, _ *mcp.CallToolRequest, input queryResultInput) (*mcp.CallToolResult, videogen.QueryResultResult, error) {
		relative := strings.TrimSpace(input.DownloadSubdir)
		if relative == "" {
			relative = relativeResultDirectory(input.ThreadID, input.RunID)
		}
		downloadDir, err := resolveWorkspacePath(r.workspace, relative)
		if err != nil {
			return nil, videogen.QueryResultResult{}, err
		}
		result, err := videogen.QueryResult(ctx, &videogen.QueryResultOptions{
			ThreadID:    strings.TrimSpace(input.ThreadID),
			RunID:       strings.TrimSpace(input.RunID),
			DownloadDir: downloadDir,
		}, r.runner)
		if err != nil {
			return nil, videogen.QueryResultResult{}, err
		}
		return nil, *result, nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_video_super_resolution",
		"Upscale video with Xiaoyunque",
		"Upload exactly one ChatGPT video and submit Xiaoyunque video super-resolution.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input videoSuperResolutionInput) (*mcp.CallToolResult, submitRunOutput, error) {
		files, cleanup, err := r.prepareFiles(ctx, input.Files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		defer cleanup()
		file, err := requireSingleFile(files, fileKindVideo)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		result, err := videotool.RunSuperResolution(ctx, &videotool.SuperResolutionOptions{
			VideoPath:        file.Path,
			ToolVersion:      input.ToolVersion,
			OutputResolution: input.OutputResolution,
		}, r.runner)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		return nil, submitOutput(result.ThreadID, result.RunID, result.WebThreadLink, nil), nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_erase_video_subtitle",
		"Erase video subtitles with Xiaoyunque",
		"Upload exactly one ChatGPT video and submit Xiaoyunque subtitle removal.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input eraseVideoSubtitleInput) (*mcp.CallToolResult, submitRunOutput, error) {
		files, cleanup, err := r.prepareFiles(ctx, input.Files)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		defer cleanup()
		file, err := requireSingleFile(files, fileKindVideo)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		result, err := videotool.RunEraseSubtitle(ctx, &videotool.EraseSubtitleOptions{VideoPath: file.Path}, r.runner)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		return nil, submitOutput(result.ThreadID, result.RunID, result.WebThreadLink, nil), nil
	})
}

func submitOutput(threadID, runID, webThreadLink string, uploaded []string) submitRunOutput {
	return submitRunOutput{
		ThreadID:         threadID,
		RunID:            runID,
		WebThreadLink:    webThreadLink,
		UploadedAssetIDs: append([]string(nil), uploaded...),
	}
}
