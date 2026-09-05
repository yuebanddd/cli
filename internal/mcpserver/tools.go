package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = `This server exposes Pippit / Xiaoyunque creative operations.

ChatGPT has native image generation. Prefer ChatGPT's native image generation when the user asks to create a new reference image. When the user then says “use this image”, “animate the image above”, or equivalent, pass the image from the current conversation in the files parameter of pippit_generate_video or pippit_nest_submit. Do not regenerate the reference image in Pippit unless the user explicitly asks to use Pippit's image model.

Generation and editing calls can consume the user's Xiaoyunque credits. Only submit a generation or editing run when the user has clearly requested the action. Poll with pippit_get_thread or pippit_query_result instead of resubmitting a job. Revisions should reuse the existing thread_id whenever the selected tool supports it.`

type toolRegistrar struct {
	runner    *common.Runner
	workspace string
	files     *filePreparer
}

// NewServer constructs the MCP server and registers every remotely useful CLI
// operation. Host administration commands such as login, logout, update, and
// starting the MCP listener remain local CLI commands.
func NewServer(runner *common.Runner, workspace string) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "pippit-tool-cli", Version: version.Current()},
		&mcp.ServerOptions{Instructions: serverInstructions},
	)
	registrar := &toolRegistrar{
		runner:    runner,
		workspace: workspace,
		files:     newFilePreparer(workspace),
	}
	registrar.registerWorkflowTools(server)
	registrar.registerMediaTools(server)
	registrar.registerCanvasTools(server)
	return server
}

func toolDefinition(name, title, description string, readOnly, destructive, openWorld bool, fileParams ...string) *mcp.Tool {
	meta := mcp.Meta{
		"openai/toolInvocation/invoking": invokingText(title),
		"openai/toolInvocation/invoked":  invokedText(title),
	}
	if len(fileParams) > 0 {
		meta["openai/fileParams"] = append([]string(nil), fileParams...)
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
			OpenWorldHint:   boolPointer(openWorld),
			IdempotentHint:  readOnly,
		},
	}
}

func invokingText(title string) string {
	return "正在执行：" + strings.TrimSpace(title) + "…"
}

func invokedText(title string) string {
	return "已完成：" + strings.TrimSpace(title)
}

func boolPointer(value bool) *bool { return &value }

func (r *toolRegistrar) prepareFiles(ctx context.Context, refs []FileReference) ([]preparedFile, func(), error) {
	return r.files.prepare(ctx, refs)
}

func (r *toolRegistrar) uploadPreparedFiles(ctx context.Context, files []preparedFile) ([]string, error) {
	assetIDs := make([]string, 0, len(files))
	for _, file := range files {
		result, err := common.UploadFile(ctx, common.UploadFileOptions{
			Path:     file.Path,
			FileName: file.FileName,
		}, r.runner)
		if err != nil {
			return nil, fmt.Errorf("upload %s to Xiaoyunque: %w", file.FileName, err)
		}
		assetIDs = append(assetIDs, result.AssetID)
	}
	return assetIDs, nil
}

func splitPreparedFiles(files []preparedFile) (images, videos, audios, documents []string, err error) {
	for _, file := range files {
		switch file.Kind {
		case fileKindImage:
			images = append(images, file.Path)
		case fileKindVideo:
			videos = append(videos, file.Path)
		case fileKindAudio:
			audios = append(audios, file.Path)
		case fileKindDocument:
			documents = append(documents, file.Path)
		default:
			return nil, nil, nil, nil, fmt.Errorf("unsupported file type for %s: %s", file.FileName, file.MIMEType)
		}
	}
	return images, videos, audios, documents, nil
}

func requireSingleFile(files []preparedFile, kind fileKind) (preparedFile, error) {
	if len(files) != 1 {
		return preparedFile{}, fmt.Errorf("exactly one file is required; received %d", len(files))
	}
	if kind != "" && files[0].Kind != kind {
		return preparedFile{}, fmt.Errorf("expected a %s file, got %s (%s)", kind, files[0].Kind, files[0].MIMEType)
	}
	return files[0], nil
}

func relativeResultDirectory(threadID, runID string) string {
	threadID = safePathSegment(threadID)
	runID = safePathSegment(runID)
	return filepath.Join("results", threadID, runID)
}

func safePathSegment(value string) string {
	value = strings.TrimSpace(value)
	value = unsafeFileName.ReplaceAllString(value, "_")
	value = strings.Trim(value, "._-")
	if value == "" {
		return "unknown"
	}
	return value
}

func newRequestID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}
