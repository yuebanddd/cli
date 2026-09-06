package generate_video

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/common"
)

// ResultLinksOptions identifies one direct generation run without requesting
// that its media artifacts be downloaded to local disk.
type ResultLinksOptions struct {
	ThreadID string
	RunID    string
}

// ResultLinksResult is the remote-only result shape used by public MCP/App
// deployments. Large media stays hosted by Xiaoyunque and only URLs/metadata
// are returned to the caller.
type ResultLinksResult struct {
	Completed    bool              `json:"completed"`
	ThreadID     string            `json:"thread_id"`
	RunID        string            `json:"run_id"`
	ErrorMessage string            `json:"error_message,omitempty"`
	Videos       []ResultLinkVideo `json:"videos"`
	Images       []ResultLinkImage `json:"images"`
}

type ResultLinkVideo struct {
	DownloadURL string `json:"download_url"`
	Title       string `json:"title,omitempty"`
	VID         string `json:"vid,omitempty"`
	AssetID     string `json:"asset_id,omitempty"`
}

type ResultLinkImage struct {
	DownloadURL string `json:"download_url"`
	AssetID     string `json:"asset_id,omitempty"`
	Format      string `json:"format,omitempty"`
}

// QueryResultLinks queries Xiaoyunque for result metadata only. It never
// downloads generated images or videos to local disk.
func QueryResultLinks(ctx context.Context, opts *ResultLinksOptions, runner *common.Runner) (*ResultLinksResult, error) {
	if opts == nil {
		return nil, fmt.Errorf("thread_id is required")
	}
	threadID := strings.TrimSpace(opts.ThreadID)
	runID := strings.TrimSpace(opts.RunID)
	if threadID == "" {
		return nil, fmt.Errorf("thread_id is required")
	}
	if runID == "" {
		return nil, fmt.Errorf("run_id is required")
	}

	threadResult, err := common.GetThread(ctx, &common.GetThreadOptions{
		ThreadID: threadID,
		RunID:    runID,
	}, runner)
	if err != nil {
		if result, ok := queryResultFromGetThreadBusinessError(err, &QueryResultOptions{ThreadID: threadID, RunID: runID}); ok {
			return &ResultLinksResult{
				Completed:    result.Completed,
				ThreadID:     result.ThreadID,
				RunID:        result.RunID,
				ErrorMessage: result.ErrorMessage,
				Videos:       []ResultLinkVideo{},
				Images:       []ResultLinkImage{},
			}, nil
		}
		return nil, fmt.Errorf("query run: %w", err)
	}

	thread, err := parseQueryThread(threadResult)
	if err != nil {
		return nil, fmt.Errorf("query run: %w", err)
	}
	run, ok := findQueryRun(thread, runID)
	if !ok {
		return nil, fmt.Errorf("run_id=%s not found", runID)
	}

	result := &ResultLinksResult{
		Completed: run.State == successRunState || run.State == failedRunState,
		ThreadID:  firstNonEmpty(thread.ThreadID, threadID),
		RunID:     runID,
		Videos:    []ResultLinkVideo{},
		Images:    []ResultLinkImage{},
	}
	if run.State == failedRunState {
		result.ErrorMessage = firstNonEmpty(extractQueryErrorMessage(run), "Run 失败")
		return result, nil
	}
	if run.State != successRunState {
		return result, nil
	}

	for _, video := range extractQueryVideos(run) {
		if strings.TrimSpace(video.DownloadURL) == "" {
			continue
		}
		result.Videos = append(result.Videos, ResultLinkVideo{
			DownloadURL: video.DownloadURL,
			Title:       video.Title,
			VID:         video.VID,
			AssetID:     video.AssetID,
		})
	}
	for _, image := range extractQueryImages(run) {
		if strings.TrimSpace(image.DownloadURL) == "" {
			continue
		}
		result.Images = append(result.Images, ResultLinkImage{
			DownloadURL: image.DownloadURL,
			AssetID:     image.AssetID,
			Format:      image.Metadata.Format,
		})
	}
	return result, nil
}
