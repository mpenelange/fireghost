package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrEntryTooLarge reports that a response body exceeds the configured bound.
var ErrEntryTooLarge = errors.New("cache entry exceeds maximum body size")

// ErrTotalBytesExceeded reports that one encoded entry cannot fit in the cache.
var ErrTotalBytesExceeded = errors.New("encoded cache entry exceeds total byte limit")

// Entry is the exact HTTP response representation stored by a Cache.
type Entry struct {
	Status      int
	ContentType string
	Body        []byte
	Warning     string `json:",omitempty"`
	ExpiresAt   time.Time
}

// Cache stores bounded response entries by an opaque key.
type Cache interface {
	Get(context.Context, string) (Entry, bool, error)
	Set(context.Context, string, Entry) error
}

// MemoryCache is an in-process cache primarily useful in tests and ephemeral deployments.
type MemoryCache struct {
	mu       sync.Mutex
	entries  map[string]Entry
	clock    func() time.Time
	maxBytes int
}

// NewMemory creates an empty memory cache.
func NewMemory(clock func() time.Time, maxEntryBytes int) *MemoryCache {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryCache{entries: make(map[string]Entry), clock: clock, maxBytes: maxEntryBytes}
}

func (c *MemoryCache) Get(ctx context.Context, key string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, found := c.entries[key]
	if !found {
		return Entry{}, false, nil
	}
	if !c.clock().Before(entry.ExpiresAt) {
		delete(c.entries, key)
		return Entry{}, false, nil
	}
	entry.Body = bytes.Clone(entry.Body)
	return entry, true, nil
}

func (c *MemoryCache) Set(ctx context.Context, key string, entry Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.maxBytes >= 0 && len(entry.Body) > c.maxBytes {
		return ErrEntryTooLarge
	}
	entry.Body = bytes.Clone(entry.Body)
	c.mu.Lock()
	c.entries[key] = entry
	c.mu.Unlock()
	return nil
}

// FileCache persists each entry as a sharded file.
type FileCache struct {
	directory string
	clock     func() time.Time
	maxBytes  int
	maxTotal  int64
	mu        sync.RWMutex
}

// NewFile opens or creates a file cache rooted at directory.
func NewFile(directory string, clock func() time.Time, maxEntryBytes int, maxTotalBytes ...int64) (*FileCache, error) {
	if clock == nil {
		clock = time.Now
	}
	maxTotal := int64(1 << 30)
	if len(maxTotalBytes) > 0 {
		maxTotal = maxTotalBytes[0]
	}
	if maxTotal <= 0 {
		return nil, fmt.Errorf("total cache byte limit must be positive")
	}
	if info, err := os.Lstat(directory); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("cache directory must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect cache directory: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	return &FileCache{directory: directory, clock: clock, maxBytes: maxEntryBytes, maxTotal: maxTotal}, nil
}

func (c *FileCache) Get(ctx context.Context, key string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	path, err := c.path(key)
	if err != nil {
		return Entry{}, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	info, statErr := os.Lstat(path)
	if statErr == nil && !info.Mode().IsRegular() {
		_ = os.Remove(path)
		return Entry{}, false, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("read cache entry: %w", err)
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, false, fmt.Errorf("decode cache entry: %w", err)
	}
	if c.maxBytes >= 0 && len(entry.Body) > c.maxBytes {
		return Entry{}, false, ErrEntryTooLarge
	}
	now := c.clock()
	if !now.Before(entry.ExpiresAt) {
		_ = os.Remove(path)
		return Entry{}, false, nil
	}
	_ = os.Chtimes(path, now, now)
	return entry, true, nil
}

func (c *FileCache) Set(ctx context.Context, key string, entry Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.maxBytes >= 0 && len(entry.Body) > c.maxBytes {
		return ErrEntryTooLarge
	}
	path, err := c.path(key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode cache entry: %w", err)
	}
	if int64(len(data)) > c.maxTotal {
		return ErrTotalBytesExceeded
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, total, err := c.scanLocked()
	if err != nil {
		return err
	}
	shard := filepath.Dir(path)
	if info, err := os.Lstat(shard); err == nil && !info.IsDir() {
		if err := os.Remove(shard); err != nil {
			return fmt.Errorf("remove unsafe cache shard: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect cache shard: %w", err)
	}
	for _, existing := range entries {
		if existing.path == path {
			total -= existing.size
		}
	}
	for _, existing := range entries {
		if total+int64(len(data)) <= c.maxTotal {
			break
		}
		if existing.path == path {
			continue
		}
		if err := os.Remove(existing.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("evict cache entry: %w", err)
		}
		total -= existing.size
	}
	if err := os.MkdirAll(shard, 0o700); err != nil {
		return fmt.Errorf("create cache shard: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary cache entry: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err != nil {
		return fmt.Errorf("write cache entry: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close cache entry: %w", closeErr)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace cache entry: %w", err)
	}
	return nil
}

type diskEntry struct {
	path    string
	size    int64
	modTime time.Time
}

func (c *FileCache) scanLocked() ([]diskEntry, int64, error) {
	var entries []diskEntry
	var total int64
	err := filepath.WalkDir(c.directory, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect cache entry: %w", walkErr)
		}
		if path == c.directory || item.IsDir() {
			return nil
		}
		if item.Type()&os.ModeSymlink != 0 || strings.Contains(item.Name(), ".tmp-") {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove unsafe cache artifact: %w", err)
			}
			return nil
		}
		if filepath.Ext(path) != ".cache" {
			return nil
		}
		info, err := item.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var entry Entry
		if err := json.Unmarshal(data, &entry); err != nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove corrupt cache entry: %w", err)
			}
			return nil
		}
		if c.clock().Before(entry.ExpiresAt) {
			entries = append(entries, diskEntry{path: path, size: info.Size(), modTime: info.ModTime()})
			total += info.Size()
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove expired cache entry: %w", err)
		}
		return nil
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].path < entries[j].path
		}
		return entries[i].modTime.Before(entries[j].modTime)
	})
	return entries, total, err
}

func (c *FileCache) path(key string) (string, error) {
	if len(key) < 2 || strings.ContainsAny(key, `/\\`) || key == "." || key == ".." {
		return "", fmt.Errorf("invalid cache key")
	}
	return filepath.Join(c.directory, key[:2], key+".cache"), nil
}
