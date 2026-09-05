package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/generate_image"
	"github.com/Pippit-dev/pippit-cli/internal/generate_video"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type generateImageInput struct {
	Prompt             string      `json:"prompt" jsonschema:"image creation or editing instruction"`
	Images             []FileInput `json:"images,omitempty" jsonschema:"reference images from the current ChatGPT conversation"`
	Model              string      `json:"model" jsonschema:"Xiaoyunque image model name"`
	Ratio              string      `json:"ratio,omitempty" jsonschema:"ratio enum: 0 auto, 2 16:9, 13 21:9, 3 9:16, 4 4:3, 5 3:4, 6 1:1"`
	Resolution         string      `json:"resolution,omitempty" jsonschema:"requested image resolution"`
	GenerateImageCount *int        `json:"generate_image_count,omitempty" jsonschema:"number of images to generate"`
}

type generateVideoInput struct {
	Prompt       string      `json:"prompt" jsonschema:"video creation instruction; include the storyboard, motion, camera, duration, and desired result"`
	Images       []FileInput `json:"images,omitempty" jsonschema:"up to nine reference images, including images generated in this ChatGPT conversation"`
	Videos       []FileInput `json:"videos,omitempty" jsonschema:"up to three reference videos"`
	Audios       []FileInput `json:"audios,omitempty" jsonschema:"up to three mp3/wav reference audio files"`
	DurationSec  *int        `json:"duration_sec,omitempty" jsonschema:"requested video duration in seconds"`
	Ratio        string      `json:"ratio,omitempty" jsonschema:"requested aspect ratio such as 9:16 or 16:9"`
	Model        string      `json:"model,omitempty" jsonschema:"Xiaoyunque video model name"`
	Resolution   string      `json:"resolution,omitempty" jsonschema:"requested output resolution"`
	GenerateType *int64      `json:"generate_type,omitempty" jsonschema:"provider-specific generation type"`
}

type queryResultInput struct {
	ThreadID string `json:"thread_id"`
	RunID    string `json:"run_id"`
}

func (s *service) registerGenerationTools(server *mcp.Server) {
	mcp.AddTool(server, toolDefinition(
		"pippit_generate_image",
		"Generate image with Xiaoyunque",
		"Generate or edit images with Xiaoyunque. ChatGPT already has native image generation, but this tool preserves full CLI parity and can use current-chat reference images. This operation may consume Xiaoyunque credits.",
		false, false, false, true, []string{"images"},
		"正在向小云雀提交生图任务…", "小云雀生图任务已提交",
	), s.handleGenerateImage)

	mcp.AddTool(server, toolDefinition(
		"pippit_generate_video",
		"Generate video with Xiaoyunque",
		"Generate a Xiaoyunque video from text and optional ChatGPT-generated images, reference videos, or audio. Use the images parameter when the user says to turn an image from this conversation into video. This operation consumes Xiaoyunque credits.",
		false, false, false, true, []string{"images", "videos", "audios"},
		"正在向小云雀提交视频任务…", "小云雀视频任务已提交",
	), s.handleGenerateVideo)

	mcp.AddTool(server, toolDefinition(
		"pippit_query_result",
		"Query generated media links",
		"Query a Xiaoyunque generation run and return completion state plus Xiaoyunque-hosted image/video URLs and metadata. Large generated media is not downloaded or proxied by the MCP server.",
		true, false, true, true, nil,
		"正在查询小云雀生成结果…", "已获取小云雀生成结果",
	), s.handleQueryResult)
}

func (s *service) handleGenerateImage(ctx context.Context, _ *mcp.CallToolRequest, input generateImageInput) (*mcp.CallToolResult, submitRunOutput, error) {
	input.Prompt = strings.TrimSpace(input.Prompt)
	input.Model = strings.TrimSpace(input.Model)
	if input.Prompt == "" {
		return nil, submitRunOutput{}, fmt.Errorf("prompt is required")
	}
	if input.Model == "" {
		return nil, submitRunOutput{}, fmt.Errorf("model is required")
	}
	materialized, err := s.downloader.materialize(ctx, input.Images, fileKindImage)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	defer materialized.Cleanup()
	result, err := generate_image.Run(ctx, &generate_image.Options{
		Prompt:             input.Prompt,
		ImagePaths:         materialized.Paths,
		Model:              input.Model,
		Ratio:              input.Ratio,
		Resolution:         input.Resolution,
		GenerateImageCount: input.GenerateImageCount,
	}, s.runner)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	return nil, submitRunOutput{
		ThreadID:      result.ThreadID,
		RunID:         result.RunID,
		WebThreadLink: result.WebThreadLink,
	}, nil
}

func (s *service) handleGenerateVideo(ctx context.Context, _ *mcp.CallToolRequest, input generateVideoInput) (*mcp.CallToolResult, submitRunOutput, error) {
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" {
		return nil, submitRunOutput{}, fmt.Errorf("prompt is required")
	}
	images, err := s.downloader.materialize(ctx, input.Images, fileKindImage)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	defer images.Cleanup()
	videos, err := s.downloader.materialize(ctx, input.Videos, fileKindVideo)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	defer videos.Cleanup()
	audios, err := s.downloader.materialize(ctx, input.Audios, fileKindAudio)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	defer audios.Cleanup()

	result, err := generate_video.Run(ctx, &generate_video.Options{
		Prompt:       input.Prompt,
		ImagePaths:   images.Paths,
		VideoPaths:   videos.Paths,
		AudioPaths:   audios.Paths,
		DurationSec:  input.DurationSec,
		Ratio:        strings.TrimSpace(input.Ratio),
		Model:        strings.TrimSpace(input.Model),
		Resolution:   strings.TrimSpace(input.Resolution),
		GenerateType: input.GenerateType,
	}, s.runner)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	return nil, submitRunOutput{
		ThreadID:      result.ThreadID,
		RunID:         result.RunID,
		WebThreadLink: result.WebThreadLink,
	}, nil
}

func (s *service) handleQueryResult(ctx context.Context, _ *mcp.CallToolRequest, input queryResultInput) (*mcp.CallToolResult, generate_video.ResultLinksResult, error) {
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	input.RunID = strings.TrimSpace(input.RunID)
	if input.ThreadID == "" {
		return nil, generate_video.ResultLinksResult{}, fmt.Errorf("thread_id is required")
	}
	if input.RunID == "" {
		return nil, generate_video.ResultLinksResult{}, fmt.Errorf("run_id is required")
	}
	result, err := generate_video.QueryResultLinks(ctx, &generate_video.ResultLinksOptions{
		ThreadID: input.ThreadID,
		RunID:    input.RunID,
	}, s.runner)
	if err != nil {
		return nil, generate_video.ResultLinksResult{}, err
	}
	return nil, *result, nil
}
