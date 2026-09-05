package mcpserver

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MediaCache is per replica. Use a dedicated volume per process, with the same
// byte limits in the deployment's volume quota. Cross-replica user concurrency
// is enforced by PostgreSQL leases before any tool materializes inputs.
type MediaCache struct {
	Dir                                  string
	TTL                                  time.Duration
	MaxBytes, MinFreeBytes, MaxFileBytes int64
	MaxFiles                             int
	AllowChatGPTFakeIP                   bool
	mu                                   sync.Mutex
	reserved                             int64
	active                               map[string]bool
}

func (c *MediaCache) Prepare() error {
	if c.Dir == "" || c.TTL < 20*time.Minute || c.MaxFiles < 1 || c.MaxFiles > 20 || c.MaxFileBytes < 1 || c.MaxFileBytes > defaultMaxFileBytes || c.MaxBytes < c.MaxFileBytes || c.MinFreeBytes < 0 {
		return fmt.Errorf("invalid media cache limits")
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return err
	}
	i, e := os.Lstat(c.Dir)
	if e != nil {
		return e
	}
	if !i.IsDir() || i.Mode()&os.ModeSymlink != 0 || i.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("media cache must be a private 0700 directory")
	}
	c.active = make(map[string]bool)
	return c.Cleanup()
}
func (c *MediaCache) allocate(count int) (string, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if count > c.MaxFiles {
		return "", nil, fmt.Errorf("too many input files")
	}
	reserve := int64(count) * c.MaxFileBytes
	var used int64
	err := filepath.WalkDir(c.Dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("cache contains symlink")
		}
		if !d.IsDir() {
			i, e := d.Info()
			if e != nil {
				return e
			}
			used += i.Size()
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	free, err := diskFree(c.Dir)
	if err != nil {
		return "", nil, err
	}
	if used+c.reserved+reserve > c.MaxBytes || free-c.reserved-reserve < c.MinFreeBytes {
		return "", nil, fmt.Errorf("media_cache_capacity_exceeded")
	}
	dir, err := os.MkdirTemp(c.Dir, mediaCachePrefix)
	if err != nil {
		return "", nil, err
	}
	c.reserved += reserve
	c.active[dir] = true
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			_ = os.RemoveAll(dir)
			delete(c.active, dir)
			c.reserved -= reserve
		})
	}
	return dir, release, nil
}
func (c *MediaCache) Cleanup() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, e := os.ReadDir(c.Dir)
	if e != nil {
		return e
	}
	cutoff := time.Now().Add(-c.TTL)
	for _, entry := range entries {
		path := filepath.Join(c.Dir, entry.Name())
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), mediaCachePrefix) || c.active[path] {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if info.ModTime().Before(cutoff) {
			if e = os.RemoveAll(path); e != nil {
				return e
			}
		}
	}
	return nil
}
