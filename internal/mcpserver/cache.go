package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	mediaCachePrefix = "pippit-mcp-files-"
	mediaCacheMaxAge = 6 * time.Hour
)

// cleanupStaleMediaCache is best-effort crash recovery for temporary ChatGPT
// input files. Normal requests remove their cache directory via defer; this
// sweep handles process crashes or host restarts that happen before cleanup.
func cleanupStaleMediaCache() {
	root := os.TempDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-mediaCacheMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), mediaCachePrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}
