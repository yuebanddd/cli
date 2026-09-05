package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMaxFileBytes = int64(200 << 20)
	defaultMaxFiles     = 16
)

// FileReference is the file reference shape supplied by ChatGPT for parameters
// declared in a tool's openai/fileParams metadata.
type FileReference struct {
	DownloadURL string `json:"download_url" jsonschema:"HTTPS download URL supplied by ChatGPT"`
	FileID      string `json:"file_id" jsonschema:"ChatGPT file identifier"`
	FileName    string `json:"file_name,omitempty" jsonschema:"Original file name when available"`
	MIMEType    string `json:"mime_type,omitempty" jsonschema:"Original MIME type when available"`
}

type fileKind string

const (
	fileKindImage    fileKind = "image"
	fileKindVideo    fileKind = "video"
	fileKindAudio    fileKind = "audio"
	fileKindDocument fileKind = "document"
	fileKindOther    fileKind = "other"
)

type preparedFile struct {
	Path     string
	FileName string
	MIMEType string
	Kind     fileKind
}

type filePreparer struct {
	client    *http.Client
	workspace string
	maxBytes  int64
	maxFiles  int
}

func newFilePreparer(workspace string) *filePreparer {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safeDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &filePreparer{
		client: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Minute,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many file download redirects")
				}
				return validatePublicHTTPSURL(req.URL)
			},
		},
		workspace: workspace,
		maxBytes:  defaultMaxFileBytes,
		maxFiles:  defaultMaxFiles,
	}
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid download address %q: %w", address, err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve download host %q: %w", host, err)
	}
	var lastErr error
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	for _, candidate := range ips {
		if !isPublicIP(candidate.IP) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("download host %q did not resolve to a public IP", host)
}

func validatePublicHTTPSURL(u *url.URL) error {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("ChatGPT file download URL must be HTTPS")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("ChatGPT file download URL must not target a private address")
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

func (p *filePreparer) prepare(ctx context.Context, refs []FileReference) ([]preparedFile, func(), error) {
	if len(refs) == 0 {
		return nil, func() {}, nil
	}
	if len(refs) > p.maxFiles {
		return nil, func() {}, fmt.Errorf("at most %d files may be attached to one MCP call", p.maxFiles)
	}
	if err := os.MkdirAll(p.workspace, 0o700); err != nil {
		return nil, func() {}, fmt.Errorf("create MCP workspace: %w", err)
	}
	tempDir, err := os.MkdirTemp(p.workspace, ".chatgpt-files-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create temporary file directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }

	result := make([]preparedFile, 0, len(refs))
	for index, ref := range refs {
		file, err := p.prepareOne(ctx, tempDir, index, ref)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		result = append(result, file)
	}
	return result, cleanup, nil
}

func (p *filePreparer) prepareOne(ctx context.Context, tempDir string, index int, ref FileReference) (preparedFile, error) {
	rawURL := strings.TrimSpace(ref.DownloadURL)
	u, err := url.Parse(rawURL)
	if err != nil {
		return preparedFile{}, fmt.Errorf("parse ChatGPT file %q URL: %w", ref.FileID, err)
	}
	if err := validatePublicHTTPSURL(u); err != nil {
		return preparedFile{}, fmt.Errorf("ChatGPT file %q: %w", ref.FileID, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return preparedFile{}, fmt.Errorf("create ChatGPT file %q download request: %w", ref.FileID, err)
	}
	req.Header.Set("Accept", "*/*")
	resp, err := p.client.Do(req)
	if err != nil {
		return preparedFile{}, fmt.Errorf("download ChatGPT file %q: %w", ref.FileID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return preparedFile{}, fmt.Errorf("download ChatGPT file %q returned HTTP %d", ref.FileID, resp.StatusCode)
	}
	if resp.ContentLength > p.maxBytes {
		return preparedFile{}, fmt.Errorf("ChatGPT file %q exceeds the %d MB limit", ref.FileID, p.maxBytes>>20)
	}

	provisional := preferredFileName(ref, u, index)
	path := filepath.Join(tempDir, provisional)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return preparedFile{}, fmt.Errorf("create temporary ChatGPT file: %w", err)
	}
	written, copyErr := io.Copy(out, io.LimitReader(resp.Body, p.maxBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return preparedFile{}, fmt.Errorf("save ChatGPT file %q: %w", ref.FileID, copyErr)
	}
	if closeErr != nil {
		return preparedFile{}, fmt.Errorf("close ChatGPT file %q: %w", ref.FileID, closeErr)
	}
	if written > p.maxBytes {
		return preparedFile{}, fmt.Errorf("ChatGPT file %q exceeds the %d MB limit", ref.FileID, p.maxBytes>>20)
	}

	mimeType := chooseMIMEType(ref.MIMEType, resp.Header.Get("Content-Type"), path)
	extension := filepath.Ext(path)
	if extension == "" {
		if ext := extensionForMIME(mimeType); ext != "" {
			newPath := path + ext
			if err := os.Rename(path, newPath); err != nil {
				return preparedFile{}, fmt.Errorf("add extension to ChatGPT file %q: %w", ref.FileID, err)
			}
			path = newPath
		}
	}

	return preparedFile{
		Path:     path,
		FileName: filepath.Base(path),
		MIMEType: mimeType,
		Kind:     classifyFile(mimeType, filepath.Ext(path)),
	}, nil
}

var unsafeFileName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func preferredFileName(ref FileReference, u *url.URL, index int) string {
	name := strings.TrimSpace(ref.FileName)
	if name == "" && u != nil {
		name = filepath.Base(strings.TrimSpace(u.Path))
	}
	if name == "" || name == "." || name == "/" {
		name = strings.TrimSpace(ref.FileID)
	}
	if name == "" {
		name = fmt.Sprintf("chatgpt-file-%d", index+1)
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = unsafeFileName.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._-")
	if name == "" {
		name = fmt.Sprintf("chatgpt-file-%d", index+1)
	}
	if len(name) > 140 {
		ext := filepath.Ext(name)
		name = strings.TrimSuffix(name, ext)
		if len(name) > 120 {
			name = name[:120]
		}
		name += ext
	}
	return fmt.Sprintf("%02d-%s", index+1, name)
}

func chooseMIMEType(hinted, header, path string) string {
	candidates := []string{hinted, header}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		mediaType, _, err := mime.ParseMediaType(candidate)
		if err == nil && mediaType != "" && mediaType != "application/octet-stream" {
			return strings.ToLower(mediaType)
		}
	}
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		buffer := make([]byte, 512)
		n, _ := io.ReadFull(file, buffer)
		if n > 0 {
			detected := http.DetectContentType(buffer[:n])
			if detected != "application/octet-stream" {
				return strings.ToLower(strings.TrimSpace(strings.Split(detected, ";")[0]))
			}
		}
	}
	if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); byExt != "" {
		return strings.ToLower(strings.TrimSpace(strings.Split(byExt, ";")[0]))
	}
	return "application/octet-stream"
}

func extensionForMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/webm":
		return ".webm"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "text/plain":
		return ".txt"
	case "application/pdf":
		return ".pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/msword":
		return ".doc"
	default:
		return ""
	}
}

func classifyFile(mimeType, extension string) fileKind {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	extension = strings.ToLower(strings.TrimSpace(extension))
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return fileKindImage
	case strings.HasPrefix(mimeType, "video/"):
		return fileKindVideo
	case strings.HasPrefix(mimeType, "audio/"), extension == ".mp3", extension == ".wav":
		return fileKindAudio
	case strings.HasPrefix(mimeType, "text/"), extension == ".doc", extension == ".docx", extension == ".pdf", extension == ".txt":
		return fileKindDocument
	default:
		return fileKindOther
	}
}

func resolveWorkspacePath(workspace, relative string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "", errors.New("output path must not be empty")
	}
	if filepath.IsAbs(relative) {
		return "", errors.New("output path must be relative to the MCP workspace")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("output path must remain inside the MCP workspace")
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve MCP workspace: %w", err)
	}
	result, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("resolve output path: %w", err)
	}
	rel, err := filepath.Rel(root, result)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("output path escapes the MCP workspace")
	}
	return result, nil
}
