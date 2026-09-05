package mcpserver

import (
	"context"
	"fmt"
	"strings"

	internaltool "github.com/Pippit-dev/pippit-cli/internal/video_tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type videoSuperResolutionInput struct {
	Videos           []FileInput `json:"videos" jsonschema:"exactly one video from the current ChatGPT conversation"`
	OutputResolution string      `json:"output_resolution" jsonschema:"target resolution: 720p, 1080p, 2k, or 4k"`
	ToolVersion      string      `json:"tool_version,omitempty" jsonschema:"optional processing version: standard, professional_v1, or professional_v2"`
}

type eraseVideoSubtitleInput struct {
	Videos []FileInput `json:"videos" jsonschema:"exactly one video whose subtitles should be erased"`
}

func (s *service) registerVideoTools(server *mcp.Server) {
	mcp.AddTool(server, toolDefinition(
		"pippit_video_super_resolution",
		"Improve video resolution",
		"Upload one reference video and submit Xiaoyunque's video super-resolution tool. This operation may consume Xiaoyunque credits.",
		false, false, false, true, []string{"videos"},
		"正在提交视频超分任务…", "视频超分任务已提交",
	), s.handleVideoSuperResolution)

	mcp.AddTool(server, toolDefinition(
		"pippit_erase_video_subtitle",
		"Erase video subtitles",
		"Upload one video and submit Xiaoyunque's subtitle-erasing tool. This operation may consume Xiaoyunque credits.",
		false, false, false, true, []string{"videos"},
		"正在提交字幕擦除任务…", "字幕擦除任务已提交",
	), s.handleEraseVideoSubtitle)
}

func (s *service) handleVideoSuperResolution(ctx context.Context, _ *mcp.CallToolRequest, input videoSuperResolutionInput) (*mcp.CallToolResult, submitRunOutput, error) {
	if len(input.Videos) != 1 {
		return nil, submitRunOutput{}, fmt.Errorf("videos must contain exactly one video")
	}
	input.OutputResolution = strings.TrimSpace(input.OutputResolution)
	if input.OutputResolution == "" {
		return nil, submitRunOutput{}, fmt.Errorf("output_resolution is required")
	}
	materialized, err := s.downloader.materialize(ctx, input.Videos, fileKindVideo)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	defer materialized.Cleanup()
	result, err := internaltool.RunSuperResolution(ctx, &internaltool.SuperResolutionOptions{
		VideoPath:        materialized.Paths[0],
		ToolVersion:      strings.TrimSpace(input.ToolVersion),
		OutputResolution: input.OutputResolution,
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

func (s *service) handleEraseVideoSubtitle(ctx context.Context, _ *mcp.CallToolRequest, input eraseVideoSubtitleInput) (*mcp.CallToolResult, submitRunOutput, error) {
	if len(input.Videos) != 1 {
		return nil, submitRunOutput{}, fmt.Errorf("videos must contain exactly one video")
	}
	materialized, err := s.downloader.materialize(ctx, input.Videos, fileKindVideo)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	defer materialized.Cleanup()
	result, err := internaltool.RunEraseSubtitle(ctx, &internaltool.EraseSubtitleOptions{
		VideoPath: materialized.Paths[0],
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
