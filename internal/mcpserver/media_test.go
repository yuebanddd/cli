package mcpserver

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeChatGPTFileReference(t *testing.T) {
	pngHeader := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="generated image.png"`)
		_, _ = w.Write(pngHeader)
	}))
	defer server.Close()

	input := FileInput{
		DownloadURL: server.URL + "/generated.png",
		FileID:      "file_test",
		FileName:    "generated image.png",
		MIMEType:    "image/png",
	}
	downloader := newMediaDownloader(1024, true)
	files, err := downloader.materialize(context.Background(), []FileInput{input}, fileKindImage)
	if err != nil {
		t.Fatalf("materialize() error = %v", err)
	}
	defer files.Cleanup()
	if len(files.Paths) != 1 {
		t.Fatalf("materialized path count = %d", len(files.Paths))
	}
	if filepath.Ext(files.Paths[0]) != ".png" {
		t.Fatalf("materialized extension = %q", filepath.Ext(files.Paths[0]))
	}
	if info, err := os.Stat(files.Paths[0]); err != nil || info.Size() != int64(len(pngHeader)) {
		t.Fatalf("materialized file info = %#v, %v", info, err)
	}
}

func TestRemoteURLPolicyBlocksLocalNetwork(t *testing.T) {
	downloader := newMediaDownloader(1024, false)
	for _, raw := range []string{
		"http://example.com/file.png",
		"https://127.0.0.1/file.png",
		"https://169.254.169.254/latest/meta-data",
	} {
		if _, err := downloader.validateRemoteURL(raw); err == nil {
			t.Fatalf("validateRemoteURL(%q) succeeded", raw)
		}
	}
	if _, err := downloader.validateRemoteURL("https://example.com/file.png"); err != nil {
		t.Fatalf("public HTTPS URL rejected: %v", err)
	}
}

func TestPublicIPClassification(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "192.0.2.1", "::1", "fc00::1"} {
		if isPublicIP(net.ParseIP(raw)) {
			t.Fatalf("isPublicIP(%q) = true", raw)
		}
	}
	if !isPublicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address was blocked")
	}
}

func TestFileTypeValidation(t *testing.T) {
	if err := validateMaterializedFile(fileKindImage, "image/png", ".png"); err != nil {
		t.Fatalf("image rejected: %v", err)
	}
	if err := validateMaterializedFile(fileKindImage, "video/mp4", ".mp4"); err == nil {
		t.Fatal("video accepted as image")
	}
	if err := validateMaterializedFile(fileKindShortDramaDocument, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"); err != nil {
		t.Fatalf("docx rejected: %v", err)
	}
}

func TestSafeFileName(t *testing.T) {
	if got := safeFileName("../../my image?.png"); got != "my_image_.png" {
		t.Fatalf("safeFileName() = %q", got)
	}
}
