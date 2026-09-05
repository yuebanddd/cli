package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	canvascore "github.com/Pippit-dev/pippit-cli/internal/canvas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type canvasCreateInput struct {
	Title               string `json:"title,omitempty" jsonschema:"personal Canvas project title, up to 50 characters"`
	RequestID           string `json:"request_id,omitempty" jsonschema:"caller-generated request ID; generated securely when omitted"`
	Wait                bool   `json:"wait,omitempty" jsonschema:"wait for the overview artifact to become ready"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty" jsonschema:"polling interval in seconds; defaults to 1"`
	TimeoutSeconds      int    `json:"timeout_seconds,omitempty" jsonschema:"maximum wait time in seconds; defaults to 120"`
}

type canvasAllocateInput struct {
	Count int `json:"count" jsonschema:"number of asset IDs to reserve, from 1 to 5000"`
}

type canvasGetInput struct {
	AssetIDs []string `json:"asset_ids" jsonschema:"one or more Pippit Canvas asset IDs"`
}

type canvasGetOutput struct {
	RequestedAssetIDs []string `json:"requested_asset_ids"`
	Assets            []any    `json:"assets"`
	LogID             string   `json:"log_id,omitempty"`
}

type canvasApplyInput struct {
	ProjectID                   string         `json:"project_id,omitempty" jsonschema:"personal novel project ID as a positive decimal string"`
	Request                     map[string]any `json:"request" jsonschema:"Canvas BatchPatch request containing exactly one transaction"`
	AllowNonAcknowledgedResults bool           `json:"allow_non_acknowledged_results,omitempty" jsonschema:"return transport-level outcomes when a transaction is not acknowledged"`
}

type canvasUploadInput struct {
	Files               []FileInput `json:"files" jsonschema:"one or more images, videos, audio files, documents, or PDFs from the current ChatGPT conversation"`
	PollIntervalSeconds int         `json:"poll_interval_seconds,omitempty" jsonschema:"asset visibility polling interval in seconds; defaults to 1"`
	TimeoutSeconds      int         `json:"timeout_seconds,omitempty" jsonschema:"maximum asset visibility wait in seconds; defaults to 120"`
}

type canvasUploadOutput struct {
	Uploads []canvascore.UploadResult `json:"uploads"`
}

type canvasCommandDescribeInput struct {
	Command string `json:"command" jsonschema:"registered Canvas SDK command name"`
}

type canvasCommandRunInput struct {
	Command  string         `json:"command" jsonschema:"registered Canvas SDK command name"`
	CanvasID string         `json:"canvas_id" jsonschema:"target Canvas asset ID"`
	Input    map[string]any `json:"input" jsonschema:"command-specific structured input"`
}

type canvasCommandOutput struct {
	Data any    `json:"data,omitempty"`
	Raw  string `json:"raw,omitempty"`
}

func (s *service) registerCanvasTools(server *mcp.Server) {
	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_create",
		"Create Pippit Canvas",
		"Create a personal Pippit novel Canvas project. The returned IDs are durable; do not blindly repeat a request whose outcome is ambiguous.",
		false, false, false, true, nil,
		"正在创建小云雀画布…", "小云雀画布已创建",
	), s.handleCanvasCreate)

	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_allocate",
		"Allocate Canvas asset IDs",
		"Reserve unique Pippit asset IDs for assets that a later Canvas transaction will create.",
		false, false, false, true, nil,
		"正在分配画布资产 ID…", "画布资产 ID 已分配",
	), s.handleCanvasAllocate)

	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_get",
		"Get Canvas assets",
		"Read one or more Pippit Canvas assets by durable asset ID.",
		true, false, true, true, nil,
		"正在读取画布资产…", "已读取画布资产",
	), s.handleCanvasGet)

	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_apply",
		"Apply Canvas transaction",
		"Apply one atomic Canvas patch transaction. This mutates durable Canvas state and must only be called after the user has approved the intended changes.",
		false, true, false, true, nil,
		"正在应用画布事务…", "画布事务已应用",
	), s.handleCanvasApply)

	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_upload",
		"Upload files to Canvas",
		"Upload ChatGPT-generated images or user files into personal Canvas assets and wait until they are queryable.",
		false, false, false, true, []string{"files"},
		"正在上传画布素材…", "画布素材已上传",
	), s.handleCanvasUpload)

	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_command_list",
		"List Canvas SDK commands",
		"List semantic Canvas SDK commands exposed by the npm-installed pippit-tool-cli wrapper.",
		true, false, true, false, nil,
		"正在读取画布命令目录…", "已读取画布命令目录",
	), s.handleCanvasCommandList)

	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_command_describe",
		"Describe Canvas SDK command",
		"Read the parameter contract for one semantic Canvas SDK command exposed by the npm-installed wrapper.",
		true, false, true, false, nil,
		"正在读取画布命令说明…", "已读取画布命令说明",
	), s.handleCanvasCommandDescribe)

	mcp.AddTool(server, toolDefinition(
		"pippit_canvas_command_run",
		"Run Canvas SDK command",
		"Run one registered semantic Canvas SDK mutation through the npm-installed wrapper. This mutates durable Canvas state and never permits arbitrary shell commands.",
		false, true, false, true, nil,
		"正在执行画布语义命令…", "画布语义命令已执行",
	), s.handleCanvasCommandRun)
}

func (s *service) handleCanvasCreate(ctx context.Context, _ *mcp.CallToolRequest, input canvasCreateInput) (*mcp.CallToolResult, canvascore.CreateResult, error) {
	requestID := strings.TrimSpace(input.RequestID)
	if requestID == "" {
		generated, err := newCanvasRequestID()
		if err != nil {
			return nil, canvascore.CreateResult{}, err
		}
		requestID = generated
	}
	pollInterval, timeout, err := canvasDurations(input.PollIntervalSeconds, input.TimeoutSeconds)
	if err != nil {
		return nil, canvascore.CreateResult{}, err
	}
	result, err := canvascore.Create(ctx, canvascore.CreateOptions{
		Title:        strings.TrimSpace(input.Title),
		RequestID:    requestID,
		Wait:         input.Wait,
		PollInterval: pollInterval,
		WaitTimeout:  timeout,
	}, s.runner)
	if err != nil {
		if result != nil {
			return nil, *result, err
		}
		return nil, canvascore.CreateResult{}, err
	}
	return nil, *result, nil
}

func (s *service) handleCanvasAllocate(ctx context.Context, _ *mcp.CallToolRequest, input canvasAllocateInput) (*mcp.CallToolResult, canvascore.AllocateResult, error) {
	result, err := canvascore.Allocate(ctx, input.Count, s.runner)
	if err != nil {
		return nil, canvascore.AllocateResult{}, err
	}
	return nil, *result, nil
}

func (s *service) handleCanvasGet(ctx context.Context, _ *mcp.CallToolRequest, input canvasGetInput) (*mcp.CallToolResult, canvasGetOutput, error) {
	result, err := canvascore.Get(ctx, canvascore.GetOptions{AssetIDs: input.AssetIDs}, s.runner)
	if err != nil {
		return nil, canvasGetOutput{}, err
	}
	assets := make([]any, 0, len(result.Assets))
	for index, raw := range result.Assets {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, canvasGetOutput{}, fmt.Errorf("decode Canvas asset %d: %w", index+1, err)
		}
		assets = append(assets, value)
	}
	return nil, canvasGetOutput{
		RequestedAssetIDs: result.RequestedAssetIDs,
		Assets:            assets,
		LogID:             result.LogID,
	}, nil
}

func (s *service) handleCanvasApply(ctx context.Context, _ *mcp.CallToolRequest, input canvasApplyInput) (*mcp.CallToolResult, canvascore.ApplyResult, error) {
	if input.Request == nil {
		return nil, canvascore.ApplyResult{}, fmt.Errorf("request is required")
	}
	payload, err := json.Marshal(input.Request)
	if err != nil {
		return nil, canvascore.ApplyResult{}, fmt.Errorf("encode Canvas apply request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request canvascore.ApplyRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, canvascore.ApplyResult{}, fmt.Errorf("decode Canvas apply request: %w", err)
	}
	result, err := canvascore.Apply(ctx, canvascore.ApplyOptions{
		ProjectID:                   strings.TrimSpace(input.ProjectID),
		Request:                     request,
		AllowNonAcknowledgedResults: input.AllowNonAcknowledgedResults,
	}, s.runner)
	if err != nil {
		return nil, canvascore.ApplyResult{}, err
	}
	return nil, *result, nil
}

func (s *service) handleCanvasUpload(ctx context.Context, _ *mcp.CallToolRequest, input canvasUploadInput) (*mcp.CallToolResult, canvasUploadOutput, error) {
	if len(input.Files) == 0 {
		return nil, canvasUploadOutput{}, fmt.Errorf("files must contain at least one item")
	}
	pollInterval, timeout, err := canvasDurations(input.PollIntervalSeconds, input.TimeoutSeconds)
	if err != nil {
		return nil, canvasUploadOutput{}, err
	}
	materialized, err := s.downloader.materialize(ctx, input.Files, fileKindUpload)
	if err != nil {
		return nil, canvasUploadOutput{}, err
	}
	defer materialized.Cleanup()
	uploads := make([]canvascore.UploadResult, 0, len(materialized.Paths))
	for index, path := range materialized.Paths {
		result, err := canvascore.Upload(ctx, canvascore.UploadOptions{
			Path:         path,
			PollInterval: pollInterval,
			WaitTimeout:  timeout,
		}, s.runner)
		if err != nil {
			return nil, canvasUploadOutput{}, fmt.Errorf("upload Canvas file %d: %w", index+1, err)
		}
		uploads = append(uploads, *result)
	}
	return nil, canvasUploadOutput{Uploads: uploads}, nil
}

func (s *service) handleCanvasCommandList(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, canvasCommandOutput, error) {
	return s.runCanvasCommand(ctx, "list")
}

func (s *service) handleCanvasCommandDescribe(ctx context.Context, _ *mcp.CallToolRequest, input canvasCommandDescribeInput) (*mcp.CallToolResult, canvasCommandOutput, error) {
	command := strings.TrimSpace(input.Command)
	if !validCanvasCommandName(command) {
		return nil, canvasCommandOutput{}, fmt.Errorf("command must contain only letters, numbers, dot, underscore, or hyphen")
	}
	return s.runCanvasCommand(ctx, "describe", command)
}

func (s *service) handleCanvasCommandRun(ctx context.Context, _ *mcp.CallToolRequest, input canvasCommandRunInput) (*mcp.CallToolResult, canvasCommandOutput, error) {
	command := strings.TrimSpace(input.Command)
	if !validCanvasCommandName(command) {
		return nil, canvasCommandOutput{}, fmt.Errorf("command must contain only letters, numbers, dot, underscore, or hyphen")
	}
	canvasID := strings.TrimSpace(input.CanvasID)
	if canvasID == "" {
		return nil, canvasCommandOutput{}, fmt.Errorf("canvas_id is required")
	}
	if input.Input == nil {
		input.Input = map[string]any{}
	}
	payload, err := json.Marshal(input.Input)
	if err != nil {
		return nil, canvasCommandOutput{}, fmt.Errorf("encode Canvas command input: %w", err)
	}
	return s.runCanvasCommand(ctx, "run", command, "--canvas-id", canvasID, "--input", string(payload))
}

func (s *service) runCanvasCommand(ctx context.Context, args ...string) (*mcp.CallToolResult, canvasCommandOutput, error) {
	commandArgs := append([]string{"canvas", "command"}, args...)
	command := exec.CommandContext(ctx, s.options.CLICommand, commandArgs...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, canvasCommandOutput{}, fmt.Errorf("run %s canvas command: %s", s.options.CLICommand, message)
	}
	raw := strings.TrimSpace(stdout.String())
	var data any
	if raw != "" && json.Unmarshal([]byte(raw), &data) == nil {
		return nil, canvasCommandOutput{Data: data}, nil
	}
	return nil, canvasCommandOutput{Raw: raw}, nil
}

func canvasDurations(pollSeconds, timeoutSeconds int) (time.Duration, time.Duration, error) {
	if pollSeconds < 0 || timeoutSeconds < 0 {
		return 0, 0, fmt.Errorf("poll_interval_seconds and timeout_seconds must not be negative")
	}
	if pollSeconds == 0 {
		pollSeconds = 1
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = 120
	}
	return time.Duration(pollSeconds) * time.Second, time.Duration(timeoutSeconds) * time.Second, nil
}

func newCanvasRequestID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate Canvas request ID: %w", err)
	}
	return "pippit_mcp_canvas_" + hex.EncodeToString(random), nil
}

var canvasCommandNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validCanvasCommandName(command string) bool {
	return canvasCommandNamePattern.MatchString(command)
}
