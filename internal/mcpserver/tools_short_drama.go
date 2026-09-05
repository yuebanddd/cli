package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/short_drama"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type shortDramaSubmitInput struct {
	Message  string      `json:"message" jsonschema:"short-drama creation, continuation, rewriting, character, or episode instruction"`
	ThreadID string      `json:"thread_id,omitempty" jsonschema:"existing short-drama thread ID for a follow-up"`
	AssetIDs []string    `json:"asset_ids,omitempty" jsonschema:"already-uploaded Xiaoyunque document asset IDs"`
	Files    []FileInput `json:"files,omitempty" jsonschema:"optional .doc, .docx, or .txt reference documents"`
}

type shortDramaUploadInput struct {
	Files []FileInput `json:"files" jsonschema:"one or more .doc, .docx, or .txt short-drama documents"`
}

func (s *service) registerShortDramaTools(server *mcp.Server) {
	mcp.AddTool(server, toolDefinition(
		"pippit_short_drama_submit",
		"Create or revise a short drama",
		"Submit a short-drama creation or revision request. It supports an existing thread and reference documents. This operation may consume Xiaoyunque credits.",
		false, false, false, true, []string{"files"},
		"正在提交小云雀短剧任务…", "小云雀短剧任务已提交",
	), s.handleShortDramaSubmit)

	mcp.AddTool(server, toolDefinition(
		"pippit_short_drama_upload",
		"Upload short-drama documents",
		"Upload .doc, .docx, or .txt documents for a Xiaoyunque short-drama workflow and return asset IDs.",
		false, false, false, true, []string{"files"},
		"正在上传短剧文档…", "短剧文档已上传",
	), s.handleShortDramaUpload)
}

func (s *service) handleShortDramaSubmit(ctx context.Context, _ *mcp.CallToolRequest, input shortDramaSubmitInput) (*mcp.CallToolResult, submitRunOutput, error) {
	input.Message = strings.TrimSpace(input.Message)
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	if input.Message == "" {
		return nil, submitRunOutput{}, fmt.Errorf("message is required")
	}
	materialized, err := s.downloader.materialize(ctx, input.Files, fileKindShortDramaDocument)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	defer materialized.Cleanup()
	uploaded, err := s.uploadMaterialized(ctx, materialized.Paths, input.Files)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	assetIDs := normalizeAssetIDs(input.AssetIDs)
	for _, asset := range uploaded {
		assetIDs = appendUniqueString(assetIDs, asset.AssetID)
	}
	result, err := short_drama.SubmitRun(ctx, &short_drama.SubmitRunOptions{
		Message:  input.Message,
		ThreadID: input.ThreadID,
		AssetIDs: assetIDs,
	}, s.runner)
	if err != nil {
		return nil, submitRunOutput{}, err
	}
	return nil, submitRunOutput{
		ThreadID:         result.ThreadID,
		RunID:            result.RunID,
		WebThreadLink:    result.WebThreadLink,
		UploadedAssets:   uploaded,
		AttachedAssetIDs: assetIDs,
	}, nil
}

func (s *service) handleShortDramaUpload(ctx context.Context, _ *mcp.CallToolRequest, input shortDramaUploadInput) (*mcp.CallToolResult, uploadMediaOutput, error) {
	if len(input.Files) == 0 {
		return nil, uploadMediaOutput{}, fmt.Errorf("files must contain at least one document")
	}
	materialized, err := s.downloader.materialize(ctx, input.Files, fileKindShortDramaDocument)
	if err != nil {
		return nil, uploadMediaOutput{}, err
	}
	defer materialized.Cleanup()
	assets, err := s.uploadMaterialized(ctx, materialized.Paths, input.Files)
	if err != nil {
		return nil, uploadMediaOutput{}, err
	}
	return nil, uploadMediaOutput{Assets: assets}, nil
}
