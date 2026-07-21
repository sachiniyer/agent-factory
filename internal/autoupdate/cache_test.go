package autoupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestCheckCachePreservesThrottleContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), CheckCacheFileName)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	cache := NewCheckCache(path)
	if !cache.Due(config.UpdateChannelStable, "1.0.0", now) {
		t.Fatal("empty cache must be due")
	}
	if err := cache.Record(config.UpdateChannelStable, "", "v1.0.0", now); err != nil {
		t.Fatalf("record failed check: %v", err)
	}

	cache = ReadCheckCache(path)
	if cache.Due(config.UpdateChannelStable, "1.0.0", now.Add(CheckInterval-time.Second)) {
		t.Fatal("a failed attempt must close the six-hour throttle window")
	}
	if !cache.Due(config.UpdateChannelStable, "1.0.0", now.Add(CheckInterval)) {
		t.Fatal("cache must be due when the throttle window expires")
	}
	if !cache.Due(config.UpdateChannelPreview, "1.0.0", now.Add(time.Second)) {
		t.Fatal("a channel switch must invalidate the throttle")
	}
	if !cache.Due(config.UpdateChannelStable, "1.0.1", now.Add(time.Second)) {
		t.Fatal("a current-version change must invalidate the throttle")
	}

	record := cache.Channels[config.UpdateChannelStable]
	if record.LastSeenTag != "" || record.CurrentVersion != "1.0.0" {
		t.Fatalf("failed-check record = %#v, want empty tag and normalized current version", record)
	}
}

func TestReadCheckCacheTreatsLegacyAndMalformedFilesAsStale(t *testing.T) {
	for name, contents := range map[string]string{
		"legacy timestamp": "2026-07-20T12:00:00Z\n",
		"malformed JSON":   "{not-json",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), CheckCacheFileName)
			if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
				t.Fatalf("seed cache: %v", err)
			}
			cache := ReadCheckCache(path)
			if !cache.Due(config.UpdateChannelStable, "1.0.0", time.Now().UTC()) {
				t.Fatal("legacy or malformed cache must be stale")
			}
		})
	}
}

func TestTryWithCheckCachePersistsExistingJSONShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), CheckCacheFileName)
	called := false
	acquired, err := TryWithCheckCache(path, func(cache *CheckCache, now time.Time) error {
		called = true
		return cache.Record(config.UpdateChannelPreview, "v1.2.3-preview-1", "v1.2.2", now)
	})
	if err != nil {
		t.Fatalf("TryWithCheckCache: %v", err)
	}
	if !acquired || !called {
		t.Fatalf("acquired/called = %v/%v, want true/true", acquired, called)
	}

	cache := ReadCheckCache(path)
	if cache.SchemaVersion != 1 || cache.LastChannel != config.UpdateChannelPreview {
		t.Fatalf("cache header = schema %d, channel %q", cache.SchemaVersion, cache.LastChannel)
	}
	record, ok := cache.Channels[config.UpdateChannelPreview]
	if !ok {
		t.Fatalf("preview record missing from %#v", cache.Channels)
	}
	if record.LastSeenTag != "v1.2.3-preview-1" || record.CurrentVersion != "1.2.2" || record.CheckedAt.IsZero() {
		t.Fatalf("preview record = %#v", record)
	}
}
