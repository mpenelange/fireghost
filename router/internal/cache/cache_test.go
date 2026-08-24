package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"web-retrieval/internal/cache"
)

func TestMemoryCacheExpirationBoundsAndCopies(t *testing.T) {
	now := time.Unix(100, 0)
	c := cache.NewMemory(func() time.Time { return now }, 4)
	entry := cache.Entry{Status: 200, ContentType: "x/test", Body: []byte("four"), ExpiresAt: now.Add(time.Minute)}
	if err := c.Set(context.Background(), "key", entry); err != nil {
		t.Fatal(err)
	}
	entry.Body[0] = 'X'
	got, found, err := c.Get(context.Background(), "key")
	if err != nil || !found || string(got.Body) != "four" {
		t.Fatalf("Get = (%q, %v, %v), want copied entry", got.Body, found, err)
	}
	got.Body[0] = 'Y'
	again, _, _ := c.Get(context.Background(), "key")
	if string(again.Body) != "four" {
		t.Fatalf("cache body mutated through returned value: %q", again.Body)
	}
	if err := c.Set(context.Background(), "large", cache.Entry{Body: []byte("12345")}); !errors.Is(err, cache.ErrEntryTooLarge) {
		t.Fatalf("oversize Set error = %v, want ErrEntryTooLarge", err)
	}
	now = now.Add(2 * time.Minute)
	if _, found, err := c.Get(context.Background(), "key"); err != nil || found {
		t.Fatalf("expired Get found = %v, err = %v", found, err)
	}
}

func TestFileCacheIsDurableShardedAtomicAndBounded(t *testing.T) {
	directory := t.TempDir()
	now := time.Unix(200, 0)
	clock := func() time.Time { return now }
	first, err := cache.NewFile(directory, clock, 64)
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.Entry{Status: 201, ContentType: "application/json", Body: []byte(`{"success":true}`), ExpiresAt: now.Add(time.Minute)}
	if err := first.Set(context.Background(), "abcdef", entry); err != nil {
		t.Fatal(err)
	}
	if err := first.Set(context.Background(), "abcdef", cache.Entry{Status: 202, Body: []byte("replacement"), ExpiresAt: entry.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	second, err := cache.NewFile(directory, clock, 64)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := second.Get(context.Background(), "abcdef")
	if err != nil || !found || got.Status != 202 || string(got.Body) != "replacement" {
		t.Fatalf("reopened Get = (%+v, %v, %v)", got, found, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "ab", "abcdef.cache")); err != nil {
		t.Fatalf("sharded cache file: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(directory, "ab", "*.tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files after atomic replacement = %v, err = %v", matches, err)
	}
	if err := second.Set(context.Background(), "too-large", cache.Entry{Body: make([]byte, 65)}); !errors.Is(err, cache.ErrEntryTooLarge) {
		t.Fatalf("oversize Set error = %v, want ErrEntryTooLarge", err)
	}
	now = now.Add(2 * time.Minute)
	if _, found, err := second.Get(context.Background(), "abcdef"); err != nil || found {
		t.Fatalf("expired file entry found = %v, err = %v", found, err)
	}
}

func TestFileCacheWarningSerializationIsBackwardCompatible(t *testing.T) {
	directory := t.TempDir()
	now := time.Unix(250, 0)
	c, err := cache.NewFile(directory, func() time.Time { return now }, 1024)
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.Entry{Status: 502, ContentType: "text/plain", Body: []byte("truthful"), Warning: "cloud skipped", ExpiresAt: now.Add(time.Hour)}
	if err := c.Set(context.Background(), "warning-key", entry); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.Get(context.Background(), "warning-key")
	if err != nil || !found || got.Warning != entry.Warning {
		t.Fatalf("warning round trip = (%q, %v, %v), want %q", got.Warning, found, err, entry.Warning)
	}

	legacyJSON, err := json.Marshal(struct {
		Status      int
		ContentType string
		Body        []byte
		ExpiresAt   time.Time
	}{Status: 200, ContentType: "application/json", Body: []byte(`{"success":true}`), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var legacy cache.Entry
	if err := json.Unmarshal(legacyJSON, &legacy); err != nil {
		t.Fatalf("decode legacy entry: %v", err)
	}
	if legacy.Warning != "" || legacy.Status != 200 || string(legacy.Body) != `{"success":true}` {
		t.Fatalf("decoded legacy entry = %+v", legacy)
	}
}

func TestFileCacheSetPurgesExpiredEntriesWithoutGet(t *testing.T) {
	directory := t.TempDir()
	now := time.Unix(300, 0)
	c, err := cache.NewFile(directory, func() time.Time { return now }, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(context.Background(), "expired", cache.Entry{Body: []byte("old"), ExpiresAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	expiredPath := filepath.Join(directory, "ex", "expired.cache")
	now = now.Add(2 * time.Second)
	if err := c.Set(context.Background(), "current", cache.Entry{Body: []byte("new"), ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(expiredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired entry still exists after Set: %v", err)
	}
}

func TestFileCacheEvictsOldestEntriesToTotalByteLimit(t *testing.T) {
	directory := t.TempDir()
	now := time.Unix(400, 0)
	entry := cache.Entry{Status: 200, Body: []byte("same"), ExpiresAt: now.Add(time.Hour)}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.NewFile(directory, func() time.Time { return now }, 64, int64(2*len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	for index, key := range []string{"oldest", "middle", "newest"} {
		if err := c.Set(context.Background(), key, entry); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, key[:2], key+".cache")
		mtime := now.Add(time.Duration(index) * time.Second)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := c.Get(context.Background(), "oldest"); err != nil || found {
		t.Fatalf("oldest Get found=%v err=%v, want evicted", found, err)
	}
	for _, key := range []string{"middle", "newest"} {
		if _, found, err := c.Get(context.Background(), key); err != nil || !found {
			t.Fatalf("%s Get found=%v err=%v, want retained", key, found, err)
		}
	}
}

func TestFileCacheRejectsEntryLargerThanTotalWithoutEviction(t *testing.T) {
	directory := t.TempDir()
	now := time.Unix(500, 0)
	valid := cache.Entry{Body: []byte("keep"), ExpiresAt: now.Add(time.Hour)}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.NewFile(directory, func() time.Time { return now }, 1024, int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(context.Background(), "valid", valid); err != nil {
		t.Fatal(err)
	}
	err = c.Set(context.Background(), "oversized", cache.Entry{Body: []byte("this is larger"), ExpiresAt: valid.ExpiresAt})
	if !errors.Is(err, cache.ErrTotalBytesExceeded) {
		t.Fatalf("oversized total Set error = %v, want ErrTotalBytesExceeded", err)
	}
	if _, found, err := c.Get(context.Background(), "valid"); err != nil || !found {
		t.Fatalf("valid entry after rejection found=%v err=%v", found, err)
	}
}

func TestFileCacheSetCleansArtifactsAndDoesNotFollowSymlinks(t *testing.T) {
	directory := t.TempDir()
	outside := t.TempDir()
	now := time.Unix(600, 0)
	c, err := cache.NewFile(directory, func() time.Time { return now }, 1024, 4096)
	if err != nil {
		t.Fatal(err)
	}
	corruptDir := filepath.Join(directory, "co")
	if err := os.Mkdir(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(corruptDir, "corrupt.cache")
	if err := os.WriteFile(corruptPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(directory, "abandoned.cache.tmp-123")
	if err := os.WriteFile(tempPath, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideEntry := filepath.Join(outside, "outside.cache")
	encoded, err := json.Marshal(cache.Entry{Body: []byte("secret"), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideEntry, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	linkedEntry := filepath.Join(directory, "li", "linked.cache")
	if err := os.Mkdir(filepath.Dir(linkedEntry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideEntry, linkedEntry); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.Get(context.Background(), "linked"); err != nil || found {
		t.Fatalf("symlink Get found=%v err=%v, want safe miss", found, err)
	}
	outsideShard := filepath.Join(outside, "shard")
	if err := os.Mkdir(outsideShard, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideShard, filepath.Join(directory, "sa")); err != nil {
		t.Fatal(err)
	}
	entry := cache.Entry{Body: []byte("inside"), ExpiresAt: now.Add(time.Hour)}
	if err := c.Set(context.Background(), "safe", entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outsideShard, "safe.cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Set traversed shard symlink: %v", err)
	}
	for _, artifact := range []string{corruptPath, tempPath} {
		if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %q remains after Set: %v", artifact, err)
		}
	}
}

func TestFileCacheReplacementDoesNotDoubleCountOldFile(t *testing.T) {
	directory := t.TempDir()
	now := time.Unix(700, 0)
	old := cache.Entry{Body: []byte("keep"), ExpiresAt: now.Add(time.Hour)}
	replacement := cache.Entry{Body: make([]byte, 160), ExpiresAt: old.ExpiresAt}
	keeper := cache.Entry{Body: []byte("keep"), ExpiresAt: old.ExpiresAt}
	replacementData, _ := json.Marshal(replacement)
	keeperData, _ := json.Marshal(keeper)
	c, err := cache.NewFile(directory, func() time.Time { return now }, 1024, int64(len(replacementData)+len(keeperData)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(context.Background(), "replace", old); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"keeper-a", "keeper-b"} {
		if err := c.Set(context.Background(), key, keeper); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(directory, "re", "replace.cache"), now.Add(-2*time.Minute), now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"keeper-a", "keeper-b"} {
		if err := os.Chtimes(filepath.Join(directory, "ke", key+".cache"), now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Set(context.Background(), "replace", replacement); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.Get(context.Background(), "replace"); err != nil || !found {
		t.Fatalf("replacement found=%v err=%v", found, err)
	}
	keepers := 0
	for _, key := range []string{"keeper-a", "keeper-b"} {
		_, found, err := c.Get(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			keepers++
		}
	}
	if keepers != 1 {
		t.Fatalf("retained keepers = %d, want 1 within replacement byte cap", keepers)
	}
}

func TestNewFileRejectsNonpositiveTotalLimit(t *testing.T) {
	if _, err := cache.NewFile(t.TempDir(), time.Now, 64, 0); err == nil {
		t.Fatal("NewFile accepted zero total byte limit")
	}
}

func TestFileCacheGetRefreshesEvictionRecency(t *testing.T) {
	directory := t.TempDir()
	now := time.Unix(800, 0)
	entry := cache.Entry{Body: []byte("same"), ExpiresAt: now.Add(time.Hour)}
	encoded, _ := json.Marshal(entry)
	c, err := cache.NewFile(directory, func() time.Time { return now }, 64, int64(2*len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"alpha", "bravo"} {
		if err := c.Set(context.Background(), key, entry); err != nil {
			t.Fatal(err)
		}
	}
	alphaPath := filepath.Join(directory, "al", "alpha.cache")
	bravoPath := filepath.Join(directory, "br", "bravo.cache")
	if err := os.Chtimes(alphaPath, now.Add(-2*time.Minute), now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bravoPath, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.Get(context.Background(), "alpha"); err != nil || !found {
		t.Fatalf("alpha Get found=%v err=%v", found, err)
	}
	if err := c.Set(context.Background(), "charlie", entry); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := c.Get(context.Background(), "alpha"); !found {
		t.Fatal("recently accessed alpha was evicted")
	}
	if _, found, _ := c.Get(context.Background(), "bravo"); found {
		t.Fatal("less recently used bravo was retained")
	}
}
