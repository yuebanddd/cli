package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	shortdrama "github.com/Pippit-dev/pippit-cli/internal/short_drama"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type emptyInput struct{}

type authStatusOutput struct {
	LoggedIn        bool   `json:"logged_in"`
	Source          string `json:"source,omitempty"`
	UID             string `json:"uid,omitempty"`
	TokenID         string `json:"token_id,omitempty"`
	CredentialScope string `json:"credential_scope,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
}

type getThreadInput struct {
	ThreadID string `json:"thread_id" jsonschema:"Xiaoyunque thread ID"`
	RunID    string `json:"run_id,omitempty" jsonschema:"Optional run ID to select"`
}

type getThreadOutput struct {
	ReadableText string `json:"readable_text"`
	Data         any    `json:"data,omitempty"`
}

type listThreadFilesInput struct {
	ThreadID string `json:"thread_id" jsonschema:"Xiaoyunque thread ID"`
	PageSize int    `json:"page_size,omitempty" jsonschema:"Page size from 1 to 200"`
	PageNum  int    `json:"page_num,omitempty" jsonschema:"One-based page number"`
}

type downloadResultInput struct {
	URL         string `json:"url" jsonschema:"HTTPS Xiaoyunque result URL"`
	OutputPath  string `json:"output_path" jsonschema:"Relative output file path inside the MCP workspace"`
	UpdatedAt   int64  `json:"updated_at,omitempty" jsonschema:"Optional remote update time as a Unix timestamp"`
	Workers     int    `json:"workers,omitempty" jsonschema:"Download worker count; defaults to 5"`
}

type shortDramaSubmitInput struct {
	Message  string          `json:"message" jsonschema:"Instruction to the Xiaoyunque short-drama agent"`
	ThreadID string          `json:"thread_id,omitempty" jsonschema:"Existing short-drama thread ID for continuation"`
	AssetIDs []string        `json:"asset_ids,omitempty" jsonschema:"Existing Xiaoyunque asset IDs"`
	Files    []FileReference `json:"files,omitempty" jsonschema:"Reference files from the current ChatGPT conversation"`
}

type shortDramaUploadInput struct {
	Files []FileReference `json:"files" jsonschema:"Exactly one .doc, .docx, or .txt file from the current ChatGPT conversation"`
}

func (r *toolRegistrar) registerWorkflowTools(server *mcp.Server) {
	mcp.AddTool(server, toolDefinition(
		"pippit_auth_status",
		"Check Xiaoyunque login status",
		"Check whether this MCP host has a usable Xiaoyunque CLI credential. The secret is never returned.",
		true, false, false,
	), func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, authStatusOutput, error) {
		status, err := r.runner.Auth.Status(ctx)
		if err != nil {
			return nil, authStatusOutput{}, err
		}
		output := authStatusOutput{
			LoggedIn:        status.LoggedIn,
			Source:          status.Source,
			UID:             status.UID,
			TokenID:         status.TokenID,
			CredentialScope: status.CredentialScope,
		}
		if !status.ExpiresAt.IsZero() {
			output.ExpiresAt = status.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return nil, output, nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_get_thread",
		"Get Xiaoyunque thread",
		"Read a Xiaoyunque thread or run, including its human-readable progress, questions, errors, and artifact metadata. Use this for polling and conversational follow-up.",
		true, false, true,
	), func(ctx context.Context, _ *mcp.CallToolRequest, input getThreadInput) (*mcp.CallToolResult, getThreadOutput, error) {
		result, err := common.GetThread(ctx, &common.GetThreadOptions{
			ThreadID: strings.TrimSpace(input.ThreadID),
			RunID:    strings.TrimSpace(input.RunID),
			Version:  common.GetThreadVersionV2,
		}, r.runner)
		if err != nil {
			return nil, getThreadOutput{}, err
		}
		var data any
		if len(result.RawData) > 0 {
			if err := json.Unmarshal(result.RawData, &data); err != nil {
				return nil, getThreadOutput{}, fmt.Errorf("decode Xiaoyunque thread data: %w", err)
			}
		}
		return nil, getThreadOutput{ReadableText: result.ReadableText, Data: data}, nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_list_thread_files",
		"List Xiaoyunque thread files",
		"List generated or uploaded files attached to a Xiaoyunque thread, including download URLs and update timestamps.",
		true, false, true,
	), func(ctx context.Context, _ *mcp.CallToolRequest, input listThreadFilesInput) (*mcp.CallToolResult, common.ListThreadFileResult, error) {
		result, err := common.ListThreadFile(ctx, &common.ListThreadFileOptions{
			ThreadID: strings.TrimSpace(input.ThreadID),
			PageSize: input.PageSize,
			PageNum:  input.PageNum,
		}, r.runner)
		if err != nil {
			return nil, common.ListThreadFileResult{}, err
		}
		return nil, *result, nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_download_result",
		"Download Xiaoyunque result",
		"Download one HTTPS result URL into a relative path inside the MCP workspace. This mirrors the CLI download-result command and never writes outside the configured workspace.",
		false, false, true,
	), func(ctx context.Context, _ *mcp.CallToolRequest, input downloadResultInput) (*mcp.CallToolResult, common.DownloadResultResponse, error) {
		u, err := url.Parse(strings.TrimSpace(input.URL))
		if err != nil {
			return nil, common.DownloadResultResponse{}, err
		}
		if err := validatePublicHTTPSURL(u); err != nil {
			return nil, common.DownloadResultResponse{}, err
		}
		outputPath, err := resolveWorkspacePath(r.workspace, input.OutputPath)
		if err != nil {
			return nil, common.DownloadResultResponse{}, err
		}
		workers := input.Workers
		if workers <= 0 {
			workers = 5
		}
		result, err := common.DownloadResult(ctx, common.DownloadResultOptions{
			URL:        u.String(),
			OutputPath: outputPath,
			UpdatedAt:  input.UpdatedAt,
			Workers:    workers,
		}, r.runner)
		if err != nil {
			return nil, common.DownloadResultResponse{}, err
		}
		return nil, *result, nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_short_drama_submit",
		"Create or revise a short drama",
		"Submit a Xiaoyunque short-drama task. Attach scripts or reference files from ChatGPT, and reuse thread_id for continuation, rewriting, character work, or episode generation.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input shortDramaSubmitInput) (*mcp.CallToolResult, submitRunOutput, error) {
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
		result, err := shortdrama.SubmitRun(ctx, &shortdrama.SubmitRunOptions{
			Message:  strings.TrimSpace(input.Message),
			ThreadID: strings.TrimSpace(input.ThreadID),
			AssetIDs: assetIDs,
		}, r.runner)
		if err != nil {
			return nil, submitRunOutput{}, err
		}
		return nil, submitOutput(result.ThreadID, result.RunID, result.WebThreadLink, uploaded), nil
	})

	mcp.AddTool(server, toolDefinition(
		"pippit_short_drama_upload",
		"Upload short-drama document",
		"Upload exactly one .doc, .docx, or .txt document for use by the Xiaoyunque short-drama workflow.",
		false, false, true, "files",
	), func(ctx context.Context, _ *mcp.CallToolRequest, input shortDramaUploadInput) (*mcp.CallToolResult, uploadFileOutput, error) {
		files, cleanup, err := r.prepareFiles(ctx, input.Files)
		if err != nil {
			return nil, uploadFileOutput{}, err
		}
		defer cleanup()
		file, err := requireSingleFile(files, "")
		if err != nil {
			return nil, uploadFileOutput{}, err
		}
		switch strings.ToLower(filepath.Ext(file.Path)) {
		case ".doc", ".docx", ".txt":
		default:
			return nil, uploadFileOutput{}, fmt.Errorf("short-drama upload supports .doc, .docx, and .txt only")
		}
		result, err := common.UploadFile(ctx, common.UploadFileOptions{Path: file.Path, FileName: file.FileName}, r.runner)
		if err != nil {
			return nil, uploadFileOutput{}, err
		}
		return nil, uploadFileOutput{AssetID: result.AssetID, FileName: file.FileName, MIMEType: file.MIMEType}, nil
	})
}
