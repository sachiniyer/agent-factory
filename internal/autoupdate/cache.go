package autoupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	// CheckInterval is the existing release-check throttle window. Releases cut
	// several times a day, so six hours keeps clients near their channel while
	// collapsing bursts of activity into one GitHub call. Failures and successes
	// both close this window.
	CheckInterval = 6 * time.Hour
	// CheckCacheFileName is the channel-aware release-check cache.
	CheckCacheFileName = "last_update_check"
)

// CheckCache is the on-disk throttle state shared by all update triggers.
// path is deliberately omitted from JSON so the existing file format remains
// unchanged.
type CheckCache struct {
	path          string
	SchemaVersion int                    `json:"schema_version,omitempty"`
	LastChannel   string                 `json:"last_channel,omitempty"`
	Channels      map[string]CheckRecord `json:"channels,omitempty"`
}

// CheckRecord describes the last attempted check for one release channel.
type CheckRecord struct {
	CheckedAt      time.Time `json:"checked_at"`
	LastSeenTag    string    `json:"last_seen_tag,omitempty"`
	CurrentVersion string    `json:"current_version,omitempty"`
}

// CheckCachePath returns the shared throttle cache path, or "" if the AF
// configuration directory cannot be resolved.
func CheckCachePath() string {
	dir, err := config.GetConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, CheckCacheFileName)
}

// NewCheckCache returns an empty cache associated with path.
func NewCheckCache(path string) *CheckCache {
	return &CheckCache{path: path}
}

// ReadCheckCache reads path. Missing, malformed, and legacy timestamp-only
// files are treated as stale empty caches so the next caller performs a check.
func ReadCheckCache(path string) *CheckCache {
	cache := NewCheckCache(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return cache
	}
	if err := json.Unmarshal(data, cache); err == nil {
		cache.path = path
		if cache.Channels == nil {
			cache.Channels = map[string]CheckRecord{}
		}
		return cache
	}
	return cache
}

// TryWithCheckCache runs fn against the latest cache state while holding the
// non-blocking update lock. acquired is false when another process owns the
// check; fn is not called in that case.
func TryWithCheckCache(path string, fn func(cache *CheckCache, now time.Time) error) (acquired bool, err error) {
	if path == "" {
		return true, fn(NewCheckCache(""), time.Now().UTC())
	}
	return config.TryWithFileLock(path, func() error {
		return fn(ReadCheckCache(path), time.Now().UTC())
	})
}

// Due reports whether channel should perform a release check for
// currentVersion at now. A channel switch, running-version change, future
// timestamp, or absent record invalidates the throttle.
func (cache *CheckCache) Due(channel, currentVersion string, now time.Time) bool {
	channel = NormalizeChannel(channel)
	if cache == nil {
		return true
	}
	if cache.LastChannel != "" && cache.LastChannel != channel {
		return true
	}
	record, ok := cache.Channels[channel]
	if !ok {
		return true
	}
	if record.CurrentVersion != "" && record.CurrentVersion != currentVersion {
		return true
	}
	if record.CheckedAt.IsZero() || record.CheckedAt.After(now) {
		return true
	}
	return now.Sub(record.CheckedAt) >= CheckInterval
}

// Record stores one attempted check. It records failures too: LastSeenTag may
// be empty, but CheckedAt still closes the throttle window.
func (cache *CheckCache) Record(channel, lastSeenTag, currentVersion string, now time.Time) error {
	if cache == nil || cache.path == "" {
		return nil
	}
	channel = NormalizeChannel(channel)
	if cache.Channels == nil {
		cache.Channels = map[string]CheckRecord{}
	}
	cache.SchemaVersion = 1
	cache.LastChannel = channel
	cache.Channels[channel] = CheckRecord{
		CheckedAt:      now.UTC(),
		LastSeenTag:    lastSeenTag,
		CurrentVersion: strings.TrimPrefix(currentVersion, "v"),
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return config.AtomicWriteFile(cache.path, data, 0644)
}

// RecordCheck updates path under the blocking cache lock. This is for
// bookkeeping outside an already-locked update cycle.
func RecordCheck(path, channel, lastSeenTag, currentVersion string) error {
	if path == "" {
		return nil
	}
	return config.WithFileLock(path, func() error {
		return ReadCheckCache(path).Record(channel, lastSeenTag, currentVersion, time.Now().UTC())
	})
}
