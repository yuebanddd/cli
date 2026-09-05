package mcpserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
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

// FileInput is the portable file-reference shape accepted by MCP tools.
// ChatGPT file handoff normally supplies download_url and file_id. data_url and
// base64 are supported for small local clients and tests.
type FileInput struct {
	DownloadURL string `json:"download_url" jsonschema:"temporary HTTPS URL supplied by ChatGPT or another MCP client"`
	URL         string `json:"url,omitempty" jsonschema:"HTTPS URL containing the file"`
	DataURL     string `json:"data_url,omitempty" jsonschema:"data URL containing a small file"`
	Base64      string `json:"base64,omitempty" jsonschema:"base64-encoded file bytes; mime_type is required"`
	FileID      string `json:"file_id" jsonschema:"opaque client file identifier used for tracing; not used as a credential"`
	FileName    string `json:"file_name,omitempty" jsonschema:"original file name including extension"`
	MIMEType    string `json:"mime_type,omitempty" jsonschema:"file MIME type, for example image/png"`
}

type fileKind uint8

const (
	fileKindImage fileKind = iota + 1
	fileKindVideo
	fileKindAudio
	fileKindMedia
	fileKindShortDramaDocument
	fileKindUpload
)

type materializedFiles struct {
	Paths []string
	Dir   string
}

func (files *materializedFiles) Cleanup() {
	if files != nil && files.Dir != "" {
		_ = os.RemoveAll(files.Dir)
	}
}

type mediaDownloader struct {
	client                    *http.Client
	maxFileBytes              int64
	allowPrivateFileURLs      bool
	allowedRedirects          int
	serverControlledUserAgent string
}

func newMediaDownloader(maxFileBytes int64, allowPrivate bool) *mediaDownloader {
	d := &mediaDownloader{
		maxFileBytes:              maxFileBytes,
		allowPrivateFileURLs:      allowPrivate,
		allowedRedirects:          5,
		serverControlledUserAgent: "Pippit-CLI-MCP/0.1",
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.dialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          20,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	d.client = &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= d.allowedRedirects {
				return fmt.Errorf("stopped after %d redirects", d.allowedRedirects)
			}
			if _, err := d.validateRemoteURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	return d
}

func (d *mediaDownloader) materialize(ctx context.Context, inputs []FileInput, kind fileKind) (*materializedFiles, error) {
	if len(inputs) == 0 {
		return &materializedFiles{Paths: []string{}}, nil
	}
	dir, err := os.MkdirTemp("", "pippit-mcp-files-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary media directory: %w", err)
	}
	result := &materializedFiles{Dir: dir, Paths: make([]string, 0, len(inputs))}
	defer func() {
		if err != nil {
			result.Cleanup()
		}
	}()

	for index, input := range inputs {
		path, materializeErr := d.materializeOne(ctx, dir, index, input, kind)
		if materializeErr != nil {
			err = fmt.Errorf("materialize file %d: %w", index+1, materializeErr)
			return nil, err
		}
		result.Paths = append(result.Paths, path)
	}
	return result, nil
}

func (d *mediaDownloader) materializeOne(ctx context.Context, dir string, index int, input FileInput, kind fileKind) (string, error) {
	reader, contentType, suggestedName, err := d.openInput(ctx, input)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	temporaryPath := filepath.Join(dir, fmt.Sprintf("input-%03d.tmp", index+1))
	file, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temporary file: %w", err)
	}
	written, copyErr := copyWithLimit(file, reader, d.maxFileBytes)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(temporaryPath)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("close temporary file: %w", closeErr)
	}
	if written == 0 {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("file is empty")
	}

	detectedType, err := detectMIMEType(temporaryPath)
	if err != nil {
		_ = os.Remove(temporaryPath)
		return "", err
	}
	mimeType := normalizeMIMEType(firstNonEmpty(input.MIMEType, contentType, detectedType))
	name := firstNonEmpty(input.FileName, suggestedName, input.FileID)
	name = safeFileName(name)
	if filepath.Ext(name) == "" {
		name += extensionForMIME(mimeType)
	}
	if name == "" || name == "." {
		name = fmt.Sprintf("input-%03d%s", index+1, extensionForMIME(mimeType))
	}
	if err := validateMaterializedFile(kind, mimeType, filepath.Ext(name)); err != nil {
		_ = os.Remove(temporaryPath)
		return "", err
	}

	finalPath := filepath.Join(dir, fmt.Sprintf("%03d-%s", index+1, name))
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("finalize temporary file: %w", err)
	}
	return finalPath, nil
}

func (d *mediaDownloader) openInput(ctx context.Context, input FileInput) (io.ReadCloser, string, string, error) {
	sources := 0
	for _, value := range []string{input.DownloadURL, input.URL, input.DataURL, input.Base64} {
		if strings.TrimSpace(value) != "" {
			sources++
		}
	}
	if sources == 0 {
		if strings.TrimSpace(input.FileID) != "" {
			return nil, "", "", fmt.Errorf("file_id %q has no download_url; the MCP host must provide downloadable file bytes", input.FileID)
		}
		return nil, "", "", fmt.Errorf("one of download_url, url, data_url, or base64 is required")
	}
	if sources != 1 {
		return nil, "", "", fmt.Errorf("provide exactly one file source")
	}

	if rawURL := firstNonEmpty(input.DownloadURL, input.URL); rawURL != "" {
		parsed, err := d.validateRemoteURL(rawURL)
		if err != nil {
			return nil, "", "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return nil, "", "", fmt.Errorf("build file download request: %w", err)
		}
		req.Header.Set("Accept", "*/*")
		req.Header.Set("User-Agent", d.serverControlledUserAgent)
		resp, err := d.client.Do(req)
		if err != nil {
			return nil, "", "", fmt.Errorf("download file: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			return nil, "", "", fmt.Errorf("download file returned HTTP %d", resp.StatusCode)
		}
		if resp.ContentLength > d.maxFileBytes {
			resp.Body.Close()
			return nil, "", "", fmt.Errorf("file is %d bytes; maximum is %d", resp.ContentLength, d.maxFileBytes)
		}
		name := fileNameFromResponse(resp, parsed)
		return resp.Body, resp.Header.Get("Content-Type"), name, nil
	}

	if strings.TrimSpace(input.DataURL) != "" {
		contentType, payload, err := decodeDataURL(input.DataURL)
		if err != nil {
			return nil, "", "", err
		}
		if int64(len(payload)) > d.maxFileBytes {
			return nil, "", "", fmt.Errorf("decoded data URL exceeds %d bytes", d.maxFileBytes)
		}
		return io.NopCloser(strings.NewReader(string(payload))), contentType, input.FileName, nil
	}

	if strings.TrimSpace(input.MIMEType) == "" {
		return nil, "", "", fmt.Errorf("mime_type is required with base64 input")
	}
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(strings.TrimSpace(input.Base64)))
	return io.NopCloser(decoder), input.MIMEType, input.FileName, nil
}

func decodeDataURL(raw string) (string, []byte, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "data:") {
		return "", nil, fmt.Errorf("invalid data URL")
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return "", nil, fmt.Errorf("invalid data URL: missing comma")
	}
	metadata := raw[len("data:"):comma]
	payload := raw[comma+1:]
	parts := strings.Split(metadata, ";")
	contentType := "text/plain"
	if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
		contentType = strings.TrimSpace(parts[0])
	}
	isBase64 := false
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			isBase64 = true
			break
		}
	}
	if !isBase64 {
		decoded, err := url.PathUnescape(payload)
		if err != nil {
			return "", nil, fmt.Errorf("decode data URL: %w", err)
		}
		return contentType, []byte(decoded), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, fmt.Errorf("decode base64 data URL: %w", err)
	}
	return contentType, decoded, nil
}

func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, fmt.Errorf("copy file bytes: %w", err)
	}
	if written > limit {
		return written, fmt.Errorf("file exceeds %d-byte limit", limit)
	}
	return written, nil
}

func detectMIMEType(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("inspect file type: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	prefix, err := reader.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return "", fmt.Errorf("inspect file type: %w", err)
	}
	return http.DetectContentType(prefix), nil
}

func normalizeMIMEType(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "application/octet-stream"
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err == nil {
		return strings.ToLower(mediaType)
	}
	if semicolon := strings.IndexByte(raw, ';'); semicolon >= 0 {
		raw = raw[:semicolon]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

func validateMaterializedFile(kind fileKind, mimeType, extension string) error {
	mimeType = normalizeMIMEType(mimeType)
	extension = strings.ToLower(strings.TrimSpace(extension))
	isImage := strings.HasPrefix(mimeType, "image/") || isOneOf(extension, ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".svg")
	isVideo := strings.HasPrefix(mimeType, "video/") || isOneOf(extension, ".mp4", ".mov", ".m4v", ".webm", ".avi", ".mkv")
	isAudio := strings.HasPrefix(mimeType, "audio/") || isOneOf(extension, ".mp3", ".wav")
	isDocument := isOneOf(extension, ".doc", ".docx", ".txt") || isOneOf(mimeType,
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"text/plain",
	)
	isSafeUpload := isImage || isVideo || isAudio || isDocument || mimeType == "application/pdf" || extension == ".pdf"

	valid := false
	switch kind {
	case fileKindImage:
		valid = isImage
	case fileKindVideo:
		valid = isVideo
	case fileKindAudio:
		valid = isAudio && isOneOf(extension, ".mp3", ".wav")
	case fileKindMedia:
		valid = isImage || isVideo || (isAudio && isOneOf(extension, ".mp3", ".wav"))
	case fileKindShortDramaDocument:
		valid = isDocument && isOneOf(extension, ".doc", ".docx", ".txt")
	case fileKindUpload:
		valid = isSafeUpload
	}
	if !valid {
		return fmt.Errorf("file type %q with extension %q is not allowed for this tool", mimeType, extension)
	}
	return nil
}

func extensionForMIME(mimeType string) string {
	switch normalizeMIMEType(mimeType) {
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
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "application/pdf":
		return ".pdf"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "text/plain":
		return ".txt"
	default:
		return ".bin"
	}
}

var unsafeFileNameCharacters = regexp.MustCompile(`[^A-Za-z0-9._()\-\p{L}\p{N}]+`)

func safeFileName(raw string) string {
	raw = filepath.Base(strings.TrimSpace(raw))
	raw = strings.ReplaceAll(raw, "\x00", "")
	raw = unsafeFileNameCharacters.ReplaceAllString(raw, "_")
	raw = strings.Trim(raw, " ._")
	if raw == "" {
		return ""
	}
	runes := []rune(raw)
	if len(runes) > 180 {
		raw = string(runes[:180])
	}
	return raw
}

func fileNameFromResponse(resp *http.Response, parsed *url.URL) string {
	if raw := resp.Header.Get("Content-Disposition"); raw != "" {
		_, params, err := mime.ParseMediaType(raw)
		if err == nil && strings.TrimSpace(params["filename"]) != "" {
			return params["filename"]
		}
	}
	return filepath.Base(parsed.Path)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isOneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if strings.EqualFold(value, choice) {
			return true
		}
	}
	return false
}

func (d *mediaDownloader) validateRemoteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid file URL: %w", err)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("file URL must not contain user credentials")
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("file URL is missing a host")
	}
	if parsed.Scheme != "https" && !(d.allowPrivateFileURLs && parsed.Scheme == "http") {
		return nil, fmt.Errorf("file URL must use HTTPS")
	}
	if !d.allowPrivateFileURLs {
		if ip := net.ParseIP(parsed.Hostname()); ip != nil && !isPublicIP(ip) {
			return nil, fmt.Errorf("file URL resolves to a non-public IP address")
		}
	}
	return parsed, nil
}

func (d *mediaDownloader) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.allowPrivateFileURLs {
		return (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse download address: %w", err)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve download host %q: %w", host, err)
	}
	var lastErr error
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	for _, candidate := range addresses {
		if !isPublicIP(candidate.IP) {
			lastErr = fmt.Errorf("download host %q resolved to blocked address %s", host, candidate.IP)
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("download host %q has no public addresses", host)
	}
	return nil, lastErr
}

var blockedNetworks = mustNetworks(
	"0.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
	"2001:db8::/32",
)

func mustNetworks(values ...string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		result = append(result, network)
	}
	return result
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, network := range blockedNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

func (d *mediaDownloader) downloadToPath(ctx context.Context, rawURL, outputPath string, updatedAt int64) (*downloadOutput, error) {
	parsed, err := d.validateRemoteURL(rawURL)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(outputPath); err == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("output path %q is a directory", outputPath)
		}
		if updatedAt <= 0 || !info.ModTime().Before(time.Unix(updatedAt, 0)) {
			return &downloadOutput{OutputPath: outputPath, AlreadyExists: true, Bytes: info.Size()}, nil
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect output path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", d.serverControlledUserAgent)
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download result: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download result returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > d.maxFileBytes {
		return nil, fmt.Errorf("download result is %d bytes; maximum is %d", resp.ContentLength, d.maxFileBytes)
	}

	temporary, err := os.CreateTemp(filepath.Dir(outputPath), ".pippit-download-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary download: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, copyErr := copyWithLimit(temporary, resp.Body, d.maxFileBytes)
	closeErr := temporary.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close temporary download: %w", closeErr)
	}
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		return nil, fmt.Errorf("finalize download: %w", err)
	}
	return &downloadOutput{OutputPath: outputPath, Bytes: written}, nil
}

type downloadOutput struct {
	OutputPath    string `json:"output_path"`
	AlreadyExists bool   `json:"already_exists,omitempty"`
	Bytes         int64  `json:"bytes"`
}
