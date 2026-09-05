package chatgptapp

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/config"
)

const (
	maxChatGPTFiles = 12
	maxUploadBytes  = int64(200 * 1024 * 1024)
)

// OpenAIFile is the file object ChatGPT provides for an input declared through
// _meta["openai/fileParams"].
type OpenAIFile struct {
	DownloadURL string `json:"download_url"`
	FileID      string `json:"file_id"`
	MIMEType    string `json:"mime_type,omitempty"`
	FileName    string `json:"file_name,omitempty"`
}

type CreateVideoInput struct {
	Prompt string       `json:"prompt"`
	Files  []OpenAIFile `json:"files,omitempty"`
}

type ContinueVideoInput struct {
	ThreadID string       `json:"thread_id"`
	Prompt   string       `json:"prompt"`
	Files    []OpenAIFile `json:"files,omitempty"`
}

type VideoRunOutput struct {
	ThreadID         string   `json:"thread_id"`
	RunID            string   `json:"run_id"`
	WebThreadLink    string   `json:"web_thread_link"`
	UploadedAssetIDs []string `json:"uploaded_asset_ids"`
}

type GetVideoStatusInput struct {
	ThreadID string `json:"thread_id"`
	RunID    string `json:"run_id,omitempty"`
}

type VideoStatusOutput struct {
	ThreadID     string `json:"thread_id"`
	RunID        string `json:"run_id"`
	ReadableText string `json:"readable_text"`
}

// Service translates ChatGPT tool calls into the existing Xiaoyunque client
// and authentication stack used by the CLI.
type Service struct {
	runner         *common.Runner
	downloadClient *http.Client
	tempDir        string
}

func NewService(runner *common.Runner) *Service {
	return &Service{
		runner:         runner,
		downloadClient: newSafeDownloadClient(),
	}
}

func (s *Service) CreateVideo(ctx context.Context, input CreateVideoInput) (*VideoRunOutput, error) {
	return s.submitVideo(ctx, "chatgpt create_video", "", input.Prompt, input.Files)
}

func (s *Service) ContinueVideo(ctx context.Context, input ContinueVideoInput) (*VideoRunOutput, error) {
	return s.submitVideo(ctx, "chatgpt continue_video", input.ThreadID, input.Prompt, input.Files)
}

func (s *Service) GetVideoStatus(ctx context.Context, input GetVideoStatusInput) (*VideoStatusOutput, error) {
	if s == nil || s.runner == nil || s.runner.Client == nil {
		return nil, fmt.Errorf("ChatGPT App runner client is missing")
	}
	threadID := strings.TrimSpace(input.ThreadID)
	if threadID == "" {
		return nil, fmt.Errorf("thread_id is required")
	}

	result, err := common.GetThread(ctx, &common.GetThreadOptions{
		ThreadID: threadID,
		RunID:    strings.TrimSpace(input.RunID),
		Version:  common.GetThreadVersionV2,
	}, s.runner)
	if err != nil {
		return nil, err
	}
	return &VideoStatusOutput{
		ThreadID:     threadID,
		RunID:        strings.TrimSpace(input.RunID),
		ReadableText: result.ReadableText,
	}, nil
}

func (s *Service) submitVideo(ctx context.Context, command, threadID, prompt string, files []OpenAIFile) (*VideoRunOutput, error) {
	if s == nil || s.runner == nil || s.runner.Client == nil {
		return nil, fmt.Errorf("ChatGPT App runner client is missing")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if len(files) > maxChatGPTFiles {
		return nil, fmt.Errorf("at most %d files may be attached", maxChatGPTFiles)
	}

	assetIDs := make([]string, 0, len(files))
	for _, file := range files {
		assetID, err := s.uploadChatGPTFile(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("upload ChatGPT file %q: %w", file.FileID, err)
		}
		assetIDs = append(assetIDs, assetID)
	}

	body := map[string]any{
		"message": prompt,
	}
	if threadID = strings.TrimSpace(threadID); threadID != "" {
		body["thread_id"] = threadID
	}
	if len(assetIDs) > 0 {
		body["asset_ids"] = assetIDs
	}

	result, err := common.SubmitRun(ctx, command, body, s.runner)
	if err != nil {
		return nil, err
	}
	return &VideoRunOutput{
		ThreadID:         result.ThreadID,
		RunID:            result.RunID,
		WebThreadLink:    result.WebThreadLink,
		UploadedAssetIDs: assetIDs,
	}, nil
}

type uploadFileResponse struct {
	Ret    string `json:"ret"`
	Errmsg string `json:"errmsg"`
	LogID  string `json:"log_id"`
	Data   struct {
		PippitAssetID string `json:"pippit_asset_id"`
		AssetID       string `json:"asset_id"`
	} `json:"data"`
}

func (s *Service) uploadChatGPTFile(ctx context.Context, file OpenAIFile) (string, error) {
	if s.runner.Auth == nil {
		return "", fmt.Errorf("authentication manager is missing")
	}
	accessKey, err := s.runner.Auth.ResolveAccessKey(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve Xiaoyunque credential: %w", err)
	}

	path, fileName, contentType, err := s.downloadChatGPTFile(ctx, file)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)

	var response uploadFileResponse
	if err := s.runner.Client.SendMultipartRequest(
		ctx,
		s.uploadFilePath(),
		map[string]string{"accessKey": accessKey},
		common.MultipartFile{
			FieldName:   "file",
			Path:        path,
			FileName:    fileName,
			ContentType: contentType,
		},
		&response,
	); err != nil {
		return "", fmt.Errorf("upload file to Xiaoyunque: %w", err)
	}
	if response.Ret != "0" {
		message := strings.TrimSpace(response.Errmsg)
		if message == "" {
			message = "unknown error"
		}
		return "", common.NewLogIDError(
			fmt.Sprintf("Xiaoyunque upload failed: ret=%s errmsg=%s", response.Ret, message),
			response.LogID,
		)
	}
	assetID := strings.TrimSpace(response.Data.PippitAssetID)
	if assetID == "" {
		assetID = strings.TrimSpace(response.Data.AssetID)
	}
	if assetID == "" {
		return "", fmt.Errorf("Xiaoyunque upload response is missing an asset id")
	}
	return assetID, nil
}

func (s *Service) uploadFilePath() string {
	if s.runner != nil && s.runner.Config != nil && s.runner.Config.Paths != nil && s.runner.Config.Paths.UploadFile != "" {
		return s.runner.Config.Paths.UploadFile
	}
	return config.UploadFilePath
}

func (s *Service) downloadChatGPTFile(ctx context.Context, file OpenAIFile) (path, fileName, contentType string, err error) {
	if strings.TrimSpace(file.DownloadURL) == "" {
		return "", "", "", fmt.Errorf("download_url is required")
	}
	if strings.TrimSpace(file.FileID) == "" {
		return "", "", "", fmt.Errorf("file_id is required")
	}
	parsed, err := url.Parse(file.DownloadURL)
	if err != nil {
		return "", "", "", fmt.Errorf("parse download_url: %w", err)
	}
	if err := validateDownloadURL(parsed); err != nil {
		return "", "", "", err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", "", "", fmt.Errorf("build file download request: %w", err)
	}
	request.Header.Set("Accept", "image/*, video/*, audio/mpeg, audio/wav, application/octet-stream")

	client := s.downloadClient
	if client == nil {
		client = newSafeDownloadClient()
	}
	response, err := client.Do(request)
	if err != nil {
		return "", "", "", fmt.Errorf("download ChatGPT file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("download ChatGPT file returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxUploadBytes {
		return "", "", "", fmt.Errorf("file exceeds the Xiaoyunque 200 MB upload limit")
	}

	tempFile, err := os.CreateTemp(s.tempDir, "pippit-chatgpt-*")
	if err != nil {
		return "", "", "", fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := tempFile.Name()
	keep := false
	defer func() {
		closeErr := tempFile.Close()
		if err == nil && closeErr != nil {
			err = fmt.Errorf("close temporary file: %w", closeErr)
		}
		if !keep || err != nil {
			_ = os.Remove(tempPath)
		}
	}()

	written, copyErr := io.Copy(tempFile, io.LimitReader(response.Body, maxUploadBytes+1))
	if copyErr != nil {
		return "", "", "", fmt.Errorf("save ChatGPT file: %w", copyErr)
	}
	if written > maxUploadBytes {
		return "", "", "", fmt.Errorf("file exceeds the Xiaoyunque 200 MB upload limit")
	}
	if _, err = tempFile.Seek(0, io.SeekStart); err != nil {
		return "", "", "", fmt.Errorf("rewind temporary file: %w", err)
	}
	prefix := make([]byte, 512)
	n, readErr := tempFile.Read(prefix)
	if readErr != nil && readErr != io.EOF {
		return "", "", "", fmt.Errorf("inspect ChatGPT file: %w", readErr)
	}

	contentType = firstSupportedMediaType(
		file.MIMEType,
		response.Header.Get("Content-Type"),
		http.DetectContentType(prefix[:n]),
	)
	if contentType == "" {
		return "", "", "", fmt.Errorf("unsupported media type")
	}

	fileName = safeFileName(file.FileName, file.FileID, contentType)
	keep = true
	return tempPath, fileName, contentType, nil
}

func firstSupportedMediaType(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		mediaType, _, err := mime.ParseMediaType(value)
		if err != nil {
			continue
		}
		mediaType = strings.ToLower(mediaType)
		if isSupportedMediaType(mediaType) {
			return mediaType
		}
	}
	return ""
}

func isSupportedMediaType(mediaType string) bool {
	return strings.HasPrefix(mediaType, "image/") ||
		strings.HasPrefix(mediaType, "video/") ||
		mediaType == "audio/mpeg" ||
		mediaType == "audio/wav" ||
		mediaType == "audio/x-wav"
}

func safeFileName(name, fileID, mediaType string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = strings.TrimSpace(fileID)
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if filepath.Ext(name) == "" {
		if extensions, err := mime.ExtensionsByType(mediaType); err == nil && len(extensions) > 0 {
			name += extensions[0]
		}
	}
	if name == "" {
		return "chatgpt-upload.bin"
	}
	return name
}

func validateDownloadURL(parsed *url.URL) error {
	if parsed == nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" {
		return fmt.Errorf("download_url must be an absolute HTTPS URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("download_url must not contain user information")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return fmt.Errorf("download_url must use HTTPS port 443")
	}
	return nil
}

func newSafeDownloadClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safeDialContext(dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return validateDownloadURL(request.URL)
		},
	}
}

func safeDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse download address: %w", err)
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve download host: %w", err)
		}
		var lastErr error
		for _, address := range addresses {
			address = address.Unmap()
			if !isPublicAddress(address) {
				continue
			}
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			if err == nil {
				return connection, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("download host did not resolve to a public IP address")
	}
}

var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

func isPublicAddress(address netip.Addr) bool {
	if !address.IsValid() ||
		address.IsUnspecified() ||
		address.IsLoopback() ||
		address.IsPrivate() ||
		address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() ||
		address.IsMulticast() {
		return false
	}
	return !carrierGradeNAT.Contains(address)
}
