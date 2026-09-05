package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/common"
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

type uploadMediaInput struct {
	Files []FileInput `json:"files" jsonschema:"one or more images, videos, mp3/wav audio files, documents, or PDFs to upload"`
}

type uploadedAsset struct {
	AssetID  string `json:"asset_id"`
	FileID   string `json:"file_id,omitempty"`
	FileName string `json:"file_name,omitempty"`
}

type uploadMediaOutput struct {
	Assets []uploadedAsset `json:"assets"`
}

type nestSubmitInput struct {
	Message  string      `json:"message" jsonschema:"natural-language creation or editing instruction for Xiaoyunque NestAgent"`
	ThreadID string      `json:"thread_id,omitempty" jsonschema:"existing Xiaoyunque thread ID for a follow-up revision"`
	AssetIDs []string    `json:"asset_ids,omitempty" jsonschema:"already-uploaded Xiaoyunque asset IDs"`
	Files    []FileInput `json:"files,omitempty" jsonschema:"reference images, videos, or mp3/wav audio from the ChatGPT conversation"`
}

type submitRunOutput struct {
	ThreadID         string          `json:"thread_id"`
	RunID            string          `json:"run_id"`
	WebThreadLink    string          `json:"web_thread_link,omitempty"`
	UploadedAssets   []uploadedAsset `json:"uploaded_assets,omitempty"`
	AttachedAssetIDs []string        `json:"attached_asset_ids,omitempty"`
}

type getThreadInput struct {
	ThreadID string `json:"thread_id"`
	RunID    string `json:"run_id,omitempty"`
	Version  string `json:"version,omitempty" jsonschema:"API response version; defaults to v2"`
}

type getThreadOutput struct {
	ThreadID     string `json:"thread_id"`
	RunID        string `json:"run_id,omitempty"`
	ReadableText string `json:"readable_text,omitempty"`
	Data         any    `json:"data"`
}

type listThreadFilesInput struct {
	ThreadID string `json:"thread_id"`
	PageSize int    `json:"page_size,omitempty" jsonschema:"page size between 1 and 200; defaults to 200"`
	PageNum  int    `json:"page_num,omitempty" jsonschema:"one-based page number; defaults to 1"`
}

type downloadResultInput struct {
	URL       string `json:"url" jsonschema:"HTTPS result URL returned by Xiaoyunque"`
	FileName  string `json:"file_name,omitempty" jsonschema:"safe output filename; inferred from URL when omitted"`
	Subdir    string `json:"subdir,omitempty" jsonschema:"relative subdirectory under the configured MCP output directory"`
	UpdatedAt int64  `json:"updated_at,omitempty" jsonschema:"remote Unix update timestamp used to skip a current local copy"`
}

type downloadResultOutput struct {
	OutputPath    string `json:"output_path"`
	AlreadyExists bool   `json:"already_exists,omitempty"`
	Bytes         int64  `json:"bytes"`
}

func (s *service) registerCommonTools(server *mcp.Server) {
	mcp.AddTool(server, toolDefinition(
		"pippit_auth_status",
		"Check Xiaoyunque login",
		"Check whether this MCP server can use the CLI's existing Xiaoyunque/Pippit login. Secrets are never returned.",
		true, false, true, false, nil,
		"正在检查小云雀登录状态…", "已检查小云雀登录状态",
	), s.handleAuthStatus)

	mcp.AddTool(server, toolDefinition(
		"pippit_upload_media",
		"Upload media to Xiaoyunque",
		"Upload ChatGPT-generated images or user-provided media/documents to Xiaoyunque and return durable asset IDs. This does not start generation by itself.",
		false, false, false, true, []string{"files"},
		"正在上传素材到小云雀…", "素材已上传到小云雀",
	), s.handleUploadMedia)

	mcp.AddTool(server, toolDefinition(
		"pippit_nest_submit",
		"Create or revise with Xiaoyunque",
		"Send a conversational creation/editing request to Xiaoyunque NestAgent. Reference files can come directly from the current ChatGPT conversation. This operation may consume Xiaoyunque credits.",
		false, false, false, true, []string{"files"},
		"正在向小云雀提交创作任务…", "小云雀创作任务已提交",
	), s.handleNestSubmit)

	mcp.AddTool(server, toolDefinition(
		"pippit_get_thread",
		"Get Xiaoyunque thread",
		"Read the current messages, progress, and artifacts for a Xiaoyunque thread or run.",
		true, false, true, true, nil,
		"正在查询小云雀会话…", "已获取小云雀会话",
	), s.handleGetThread)

	mcp.AddTool(server, toolDefinition(
		"pippit_list_thread_files",
		"List Xiaoyunque thread files",
		"List downloadable files produced inside a Xiaoyunque thread.",
		true, false, true, true, nil,
		"正在列出小云雀产物…", "已列出小云雀产物",
	), s.handleListThreadFiles)

	mcp.AddTool(server, toolDefinition(
		"pippit_download_result",
		"Download Xiaoyunque result",
		"Download one HTTPS Xiaoyunque result into the MCP server's controlled output directory. The server blocks private-network URLs and path traversal by default.",
		false, false, true, true, nil,
		"正在下载小云雀产物…", "小云雀产物已下载",
	), s.handleDownloadResult)
}

func (s *service) handleAuthStatus(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, authStatusOutput, error) {
	status, err := s.runner.Auth.Status(ctx)
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
		output.ExpiresAt = status.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return nil, output, nil
}

func (s *service) handleUploadMedia(ctx context.Context, _ *mcp.CallToolRequest, input uploadMediaInput) (*mcp.CallToolResult, uploadMediaOutput, error) {
	if len(input.Files) == 0 {
		return nil, uploadMediaOutput{}, fmt.Errorf("files must contain at least one item")
	}
	materialized, err := s.downloader.materialize(ctx, input.Files, fileKindUpload)
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

func (s *service) handleNestSubmit(ctx context.Context, _ *mcp.CallToolRequest, input nestSubmitInput) (*mcp.CallToolResult, submitRunOutput, error) {
	input.Message = strings.TrimSpace(input.Message)
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	if input.Message == "" {
		return nil, submitRunOutput{}, fmt.Errorf("message is required")
	}

	materialized, err := s.downloader.materialize(ctx, input.Files, fileKindMedia)
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

	body := map[string]any{
		"agent_name": nestAgentName,
		"message":    input.Message,
	}
	if input.ThreadID != "" {
		body["thread_id"] = input.ThreadID
	}
	if len(assetIDs) > 0 {
		body["asset_ids"] = assetIDs
	}
	result, err := common.SubmitRun(ctx, "mcp nest submit", body, s.runner)
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

func (s *service) handleGetThread(ctx context.Context, _ *mcp.CallToolRequest, input getThreadInput) (*mcp.CallToolResult, getThreadOutput, error) {
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	input.RunID = strings.TrimSpace(input.RunID)
	input.Version = strings.TrimSpace(input.Version)
	if input.ThreadID == "" {
		return nil, getThreadOutput{}, fmt.Errorf("thread_id is required")
	}
	if input.Version == "" {
		input.Version = common.GetThreadVersionV2
	}
	result, err := common.GetThread(ctx, &common.GetThreadOptions{
		ThreadID: input.ThreadID,
		RunID:    input.RunID,
		Version:  input.Version,
	}, s.runner)
	if err != nil {
		return nil, getThreadOutput{}, err
	}
	var data any
	if len(result.RawData) > 0 {
		if err := json.Unmarshal(result.RawData, &data); err != nil {
			return nil, getThreadOutput{}, fmt.Errorf("decode Xiaoyunque thread data: %w", err)
		}
	}
	return nil, getThreadOutput{
		ThreadID:     input.ThreadID,
		RunID:        input.RunID,
		ReadableText: result.ReadableText,
		Data:         data,
	}, nil
}

func (s *service) handleListThreadFiles(ctx context.Context, _ *mcp.CallToolRequest, input listThreadFilesInput) (*mcp.CallToolResult, common.ListThreadFileResult, error) {
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	if input.ThreadID == "" {
		return nil, common.ListThreadFileResult{}, fmt.Errorf("thread_id is required")
	}
	if input.PageSize == 0 {
		input.PageSize = common.MaxListThreadFilePageSize
	}
	if input.PageSize < 1 || input.PageSize > common.MaxListThreadFilePageSize {
		return nil, common.ListThreadFileResult{}, fmt.Errorf("page_size must be between 1 and %d", common.MaxListThreadFilePageSize)
	}
	if input.PageNum == 0 {
		input.PageNum = 1
	}
	if input.PageNum < 1 {
		return nil, common.ListThreadFileResult{}, fmt.Errorf("page_num must be at least 1")
	}
	result, err := common.ListThreadFile(ctx, &common.ListThreadFileOptions{
		ThreadID: input.ThreadID,
		PageSize: input.PageSize,
		PageNum:  input.PageNum,
	}, s.runner)
	if err != nil {
		return nil, common.ListThreadFileResult{}, err
	}
	return nil, *result, nil
}

func (s *service) handleDownloadResult(ctx context.Context, _ *mcp.CallToolRequest, input downloadResultInput) (*mcp.CallToolResult, downloadResultOutput, error) {
	input.URL = strings.TrimSpace(input.URL)
	if input.URL == "" {
		return nil, downloadResultOutput{}, fmt.Errorf("url is required")
	}
	fileName := safeFileName(input.FileName)
	if fileName == "" {
		parsed, err := url.Parse(input.URL)
		if err != nil {
			return nil, downloadResultOutput{}, fmt.Errorf("invalid result URL: %w", err)
		}
		fileName = safeFileName(filepath.Base(parsed.Path))
	}
	if fileName == "" {
		fileName = "pippit-result.bin"
	}
	outputPath, err := s.outputPath(input.Subdir, fileName)
	if err != nil {
		return nil, downloadResultOutput{}, err
	}
	result, err := s.downloader.downloadToPath(ctx, input.URL, outputPath, input.UpdatedAt)
	if err != nil {
		return nil, downloadResultOutput{}, err
	}
	return nil, downloadResultOutput{
		OutputPath:    result.OutputPath,
		AlreadyExists: result.AlreadyExists,
		Bytes:         result.Bytes,
	}, nil
}

func (s *service) uploadMaterialized(ctx context.Context, paths []string, inputs []FileInput) ([]uploadedAsset, error) {
	assets := make([]uploadedAsset, 0, len(paths))
	for index, path := range paths {
		result, err := common.UploadFile(ctx, common.UploadFileOptions{Path: path}, s.runner)
		if err != nil {
			return nil, fmt.Errorf("upload file %d to Xiaoyunque: %w", index+1, err)
		}
		asset := uploadedAsset{AssetID: result.AssetID, FileName: filepath.Base(path)}
		if index < len(inputs) {
			asset.FileID = strings.TrimSpace(inputs[index].FileID)
			if name := safeFileName(inputs[index].FileName); name != "" {
				asset.FileName = name
			}
		}
		assets = append(assets, asset)
	}
	return assets, nil
}

func normalizeAssetIDs(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = appendUniqueString(result, value)
		}
	}
	return result
}

func appendUniqueString(values []string, addition string) []string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return values
	}
	for _, value := range values {
		if value == addition {
			return values
		}
	}
	return append(values, addition)
}

func (s *service) outputPath(subdir, fileName string) (string, error) {
	fileName = safeFileName(fileName)
	if fileName == "" {
		return "", fmt.Errorf("file_name is invalid")
	}
	cleanSubdir := filepath.Clean(strings.TrimSpace(subdir))
	if cleanSubdir == "." {
		cleanSubdir = ""
	}
	if filepath.IsAbs(cleanSubdir) || cleanSubdir == ".." || strings.HasPrefix(cleanSubdir, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("subdir must remain inside the MCP output directory")
	}
	candidate := filepath.Join(s.options.OutputDir, cleanSubdir, fileName)
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve output path: %w", err)
	}
	relative, err := filepath.Rel(s.options.OutputDir, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("output path escapes the MCP output directory")
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	return absolute, nil
}

func (s *service) outputDirectory(subdir string) (string, error) {
	cleanSubdir := filepath.Clean(strings.TrimSpace(subdir))
	if cleanSubdir == "." {
		cleanSubdir = ""
	}
	if filepath.IsAbs(cleanSubdir) || cleanSubdir == ".." || strings.HasPrefix(cleanSubdir, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("subdir must remain inside the MCP output directory")
	}
	candidate := filepath.Join(s.options.OutputDir, cleanSubdir)
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve output directory: %w", err)
	}
	relative, err := filepath.Rel(s.options.OutputDir, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("output directory escapes the MCP output root")
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	return absolute, nil
}
