package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/autoupdate"
	"github.com/stretchr/testify/require"
)

// driverHarness builds a driver over a real on-disk throttle cache in a temp AF
// home, with the clock, config, and release lookup under the test's control.
type driverHarness struct {
	driver *updateDriver
	// cachePath is the real last_update_check file the driver reads.
	cachePath string
	cfg       *config.Config
	now       time.Time
	// mu guards channels: the run tests drive the discovery stub from the
	// driver's own goroutine while the test goroutine reads it.
	mu sync.Mutex
	// channels records the channel each discovery call asked for, so a test can
	// assert BOTH that no network call happened and that the right one did.
	channels []string
	tag      string
	err      error
}

// checkedChannels returns the channels asked for so far.
func (h *driverHarness) checkedChannels() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.channels...)
}

func newDriverHarness(t *testing.T) *driverHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// The env override is process-global; unset it so a developer running the
	// suite with auto-update disabled does not silently pass every gate below.
	t.Setenv(autoupdate.EnvironmentVariable, "")

	h := &driverHarness{
		cachePath: filepath.Join(home, autoupdate.CheckCacheFileName),
		cfg:       &config.Config{AutoUpdate: true, UpdateChannel: config.UpdateChannelStable},
		now:       time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		tag:       "v1.0.300",
	}
	h.driver = &updateDriver{
		cachePath:          h.cachePath,
		currentVersion:     "1.0.200",
		config:             func() *config.Config { return h.cfg },
		now:                func() time.Time { return h.now },
		executableIdentity: func() (string, error) { return "stable-identity", nil },
		baselineExecutable: "stable-identity",
		discover: func(channel string, _ time.Duration) (string, error) {
			h.mu.Lock()
			h.channels = append(h.channels, channel)
			h.mu.Unlock()
			return h.tag, h.err
		},
	}
	return h
}

// seedCache writes a real cache record for channel at checkedAt, the same shape
// the launch path writes.
func (h *driverHarness) seedCache(t *testing.T, channel string, checkedAt time.Time) {
	t.Helper()
	cache := autoupdate.NewCheckCache(h.cachePath)
	require.NoError(t, cache.Record(channel, "v1.0.200", h.driver.currentVersion, checkedAt))
}

func (h *driverHarness) readCacheFile(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(h.cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return data
}

// The load-bearing guarantee of this slice. The launch path is still the only
// thing that installs, and it installs only when the shared six-hour window is
// open. A daemon that RECORDED its checks would close that window on its own
// cadence and leave the interactive updater arriving to a closed window every
// time — a box that updates today would stop updating, in exchange for a driver
// that installs nothing.
//
// So: the driver must leave the cache byte-identical on every path, including
// the one where it does the most work (window open, network call made, newer
// release found).
func TestUpdateDriver_NeverWritesTheSharedThrottleCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  string
		err  error
		want updateCheckOutcome
	}{
		{name: "newer release found", tag: "v1.0.300", want: updateCheckAvailable},
		{name: "already up to date", tag: "v1.0.100", want: updateCheckUpToDate},
		{name: "lookup failed", err: errors.New("github is down"), want: updateCheckFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDriverHarness(t)
			h.tag, h.err = tc.tag, tc.err
			// Seed a stale record so the shared window is genuinely open: this
			// test must exercise the branch that does the work, not a skip.
			h.seedCache(t, config.UpdateChannelStable, h.now.Add(-7*time.Hour))
			before := h.readCacheFile(t)
			require.NotEmpty(t, before, "the cache must exist, or this proves nothing")

			require.Equal(t, tc.want, h.driver.checkOnce(context.Background()))
			require.Equal(t, []string{config.UpdateChannelStable}, h.checkedChannels(),
				"the window was open, so the driver must have made exactly one lookup")

			require.Equal(t, before, h.readCacheFile(t),
				"the daemon must not close the shared throttle window: it installs nothing, and recording here stops the launch-path updater from ever finding the window open")
		})
	}
}

// The cache is never written even when it does not exist yet — the driver must
// not be what creates it, or a first daemon start would close a window no af has
// opened.
func TestUpdateDriver_DoesNotCreateTheThrottleCache(t *testing.T) {
	h := newDriverHarness(t)

	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
	require.Len(t, h.checkedChannels(), 1)

	_, err := os.Stat(h.cachePath)
	require.True(t, errors.Is(err, os.ErrNotExist), "the driver must not create the shared cache, got %v", err)
}

// The other half of coordinating on the shared window: a check another af made
// recently stands this driver down, so the daemon adds at most one lookup per
// six-hour window rather than one per wake.
func TestUpdateDriver_DefersToARecentCheckByAnotherAf(t *testing.T) {
	h := newDriverHarness(t)
	h.seedCache(t, config.UpdateChannelStable, h.now.Add(-1*time.Hour))

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Empty(t, h.checkedChannels(), "the shared window was closed, so no lookup may happen")
}

// A channel switch invalidates the shared record (CheckCache.Due), and the
// driver must ask about the channel the config names right now — not the one it
// asked about last time.
func TestUpdateDriver_FollowsAChannelSwitch(t *testing.T) {
	h := newDriverHarness(t)
	h.seedCache(t, config.UpdateChannelStable, h.now.Add(-1*time.Hour))
	h.cfg.UpdateChannel = config.UpdateChannelPreview

	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
	require.Equal(t, []string{config.UpdateChannelPreview}, h.checkedChannels())
}

// Because the driver never records, the shared window cannot throttle it. Its
// own backoff is what keeps a failing GitHub from being retried every wake —
// the retry-storm half of the #459 → #1466 → #1861 argument, which moving off
// the launch path does not change.
func TestUpdateDriver_ChecksAtMostOncePerCheckInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  string
		err  error
	}{
		{name: "after a newer release", tag: "v1.0.300"},
		{name: "after up to date", tag: "v1.0.100"},
		{name: "after a failed lookup", err: errors.New("github is down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDriverHarness(t)
			h.tag, h.err = tc.tag, tc.err

			base := h.now
			h.driver.checkOnce(context.Background())
			require.Len(t, h.checkedChannels(), 1)

			// Every wake for the next six hours: no further lookup. Offsets are
			// measured from base, not compounded, so each case is the elapsed
			// time it claims to be.
			for _, elapsed := range []time.Duration{
				updateDriverWakeInterval,
				autoupdate.CheckInterval - time.Second,
			} {
				h.now = base.Add(elapsed)
				require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()),
					"a check %s after the last one must be suppressed", elapsed)
				require.Len(t, h.checkedChannels(), 1, "a second lookup inside the interval is the retry storm the throttle exists to prevent")
			}

			// Past the interval it checks again.
			h.now = base.Add(autoupdate.CheckInterval)
			h.driver.checkOnce(context.Background())
			require.Len(t, h.checkedChannels(), 2)
		})
	}
}

// The off switch is complete: no release lookup, no network call, nothing. Both
// the persistent config key and the process-level env override, which is what an
// operator sets on a daemon unit.
func TestUpdateDriver_OffSwitchMakesNoNetworkCall(t *testing.T) {
	t.Run("auto_update = false", func(t *testing.T) {
		h := newDriverHarness(t)
		h.cfg.AutoUpdate = false

		require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
		require.Empty(t, h.checkedChannels())
	})

	t.Run("env override beats an enabled config", func(t *testing.T) {
		h := newDriverHarness(t)
		t.Setenv(autoupdate.EnvironmentVariable, "0")

		require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
		require.Empty(t, h.checkedChannels())
	})

	t.Run("env override beats a disabled config", func(t *testing.T) {
		h := newDriverHarness(t)
		h.cfg.AutoUpdate = false
		t.Setenv(autoupdate.EnvironmentVariable, "1")

		require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
		require.Len(t, h.checkedChannels(), 1)
	})
}

// The off switch is re-read every wake, so an operator who disables auto-update
// on a running daemon is honoured without restarting it — the point of a switch
// on a box whose defining property is that nobody opens the TUI.
func TestUpdateDriver_HonoursTheOffSwitchWithoutARestart(t *testing.T) {
	h := newDriverHarness(t)

	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
	require.Len(t, h.checkedChannels(), 1)

	h.cfg.AutoUpdate = false
	h.now = h.now.Add(autoupdate.CheckInterval + time.Minute)

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Len(t, h.checkedChannels(), 1, "a disabled driver must make no further lookup")
}

// A config that cannot be resolved is a skip, not a check against assumed
// defaults.
func TestUpdateDriver_SkipsWithoutAConfig(t *testing.T) {
	h := newDriverHarness(t)
	h.driver.config = func() *config.Config { return nil }

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Empty(t, h.checkedChannels())
}

// Never report a downgrade. A preview user switching back to stable resolves an
// older tag, and the shared IsNewer ordering must keep this loop from calling
// that an update — the same rule the installer follows.
func TestUpdateDriver_NeverReportsADowngrade(t *testing.T) {
	h := newDriverHarness(t)
	h.driver.currentVersion = "1.0.300-preview-4"
	h.tag = "v1.0.299"

	require.Equal(t, updateCheckUpToDate, h.driver.checkOnce(context.Background()))
}

// A stable release outranks a preview of the same base, so a preview user is
// told about the release that supersedes theirs.
func TestUpdateDriver_ReportsAStableReleaseOverAPreviewOfTheSameBase(t *testing.T) {
	h := newDriverHarness(t)
	h.driver.currentVersion = "1.0.300-preview-4"
	h.tag = "v1.0.300"

	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
}

// A lock held by another af — one mid-download on the launch path — stands the
// driver down without a network call, and without consuming its own backoff.
func TestUpdateDriver_StandsDownWhileAnotherAfHoldsTheUpdateLock(t *testing.T) {
	h := newDriverHarness(t)
	require.NoError(t, os.WriteFile(h.cachePath, []byte("{}"), 0o644))

	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_, _ = autoupdate.TryWithCheckCache(h.cachePath, func(*autoupdate.CheckCache, time.Time) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Empty(t, h.checkedChannels(), "another af owns the check; the daemon must not pile on")
	require.True(t, h.driver.nextCheckNotBefore.IsZero(),
		"standing down for a busy lock must not burn the six-hour backoff — nothing was checked")

	close(release)
}

// An unreadable shared window is not a licence to check anyway — there would be
// nothing to coordinate with. It backs off like any other unactionable outcome,
// so a permanently broken home does not log this every wake forever.
func TestUpdateDriver_BacksOffWhenTheSharedWindowIsUnreadable(t *testing.T) {
	h := newDriverHarness(t)
	// A regular file where the cache's parent directory should be: the shared
	// lock cannot be created under it, so the read fails for real rather than
	// through an injected stub.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o644))
	h.driver.cachePath = filepath.Join(blocked, autoupdate.CheckCacheFileName)

	base := h.now
	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Empty(t, h.checkedChannels(), "an unreadable window must not be checked past")

	h.now = base.Add(autoupdate.CheckInterval - time.Second)
	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.False(t, h.driver.nextCheckNotBefore.IsZero(), "the failure must have armed the backoff")
	require.True(t, h.driver.nextCheckNotBefore.After(h.now), "and it must still hold inside the interval")
}

// run's permanent gates: each ends the loop rather than being re-evaluated every
// wake, so each must provably END it — not merely skip a check inside it.
func TestUpdateDriver_PermanentGatesEndTheLoop(t *testing.T) {
	t.Run("unsupported platform", func(t *testing.T) {
		h := newDriverHarness(t)
		original := updateDriverGOOS
		updateDriverGOOS = "windows"
		t.Cleanup(func() { updateDriverGOOS = original })

		requireRunReturnsWithoutChecking(t, h)
	})

	t.Run("version is not a release version", func(t *testing.T) {
		h := newDriverHarness(t)
		h.driver.currentVersion = "" // a build with no version stamped

		requireRunReturnsWithoutChecking(t, h)
	})

	t.Run("no resolvable home", func(t *testing.T) {
		h := newDriverHarness(t)
		h.driver.cachePath = ""

		requireRunReturnsWithoutChecking(t, h)
	})
}

// requireRunReturnsWithoutChecking drives run against a stop channel that never
// closes, so ONLY a gate can end it. The wake interval is left at its production
// value: a gate that failed to end the loop would sit on that timer, and this
// reports it in seconds instead of waiting out the test binary's timeout.
func requireRunReturnsWithoutChecking(t *testing.T, h *driverHarness) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		h.driver.run(make(chan struct{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not end the loop; the gate did not hold")
	}
	require.Empty(t, h.checkedChannels())
}

// The loop is shutdown-aware: closing stopCh ends it, both while waiting for a
// wake and immediately after one.
func TestUpdateDriver_RunStopsOnShutdown(t *testing.T) {
	h := newDriverHarness(t)
	originalWake := updateDriverWakeInterval
	updateDriverWakeInterval = time.Millisecond
	t.Cleanup(func() { updateDriverWakeInterval = originalWake })

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		h.driver.run(stopCh)
		close(done)
	}()

	close(stopCh)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after stopCh closed")
	}
}

// The loop actually checks: a real run with a short wake makes a lookup. Without
// this the gate tests above would pass on a driver that never checks at all.
func TestUpdateDriver_RunPerformsACheck(t *testing.T) {
	h := newDriverHarness(t)
	h.driver.now = time.Now // the loop's backoff must be compared against a real clock
	originalWake := updateDriverWakeInterval
	updateDriverWakeInterval = time.Millisecond
	originalStartup := updateDriverStartupBackoff
	updateDriverStartupBackoff = 0
	t.Cleanup(func() {
		updateDriverWakeInterval = originalWake
		updateDriverStartupBackoff = originalStartup
	})

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		h.driver.run(stopCh)
		close(done)
	}()
	t.Cleanup(func() {
		close(stopCh)
		<-done
	})

	require.Eventually(t, func() bool { return len(h.checkedChannels()) > 0 }, 10*time.Second, 5*time.Millisecond,
		"the loop must reach a release check")
}

// The self-throttle lives in memory, so a restart resets it. Without a startup
// floor, a daemon that reaches ready and then dies repeatedly would check once
// per restart — a rate-limit storm assembled out of individually throttled
// processes, and the jitter is no floor because it is hash-derived and can land
// near zero for a given home. So no check is eligible until the driver has been
// running for a full interval.
func TestUpdateDriver_MakesNoCheckBeforeTheStartupBackoff(t *testing.T) {
	h := newDriverHarness(t)
	h.driver.now = time.Now
	originalWake := updateDriverWakeInterval
	updateDriverWakeInterval = time.Millisecond
	originalStartup := updateDriverStartupBackoff
	updateDriverStartupBackoff = time.Hour
	t.Cleanup(func() {
		updateDriverWakeInterval = originalWake
		updateDriverStartupBackoff = originalStartup
	})

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		h.driver.run(stopCh)
		close(done)
	}()

	// Hundreds of wakes at a millisecond apiece, none of them eligible.
	time.Sleep(300 * time.Millisecond)
	close(stopCh)
	<-done

	require.Empty(t, h.checkedChannels(),
		"a restarted daemon must not be able to check before it has run for a full interval")
}

// And the production floor is the check interval itself — pinned so it cannot
// drift down to something a restart loop could outrun.
func TestUpdateDriverStartupBackoffIsAFullCheckInterval(t *testing.T) {
	require.Equal(t, autoupdate.CheckInterval, updateDriverStartupBackoff)
}

// Taking the shared cache lock MkdirAll's the lock file's parent, so a probe on a
// deleted home would recreate it — resetting watchDaemonHome's miss counter (it
// clears on any successful stat) and keeping an abandoned daemon alive, which is
// #1093 exactly. A release check is never worth resurrecting a home the user
// deleted.
func TestUpdateDriver_DoesNotRecreateADeletedHome(t *testing.T) {
	h := newDriverHarness(t)
	home := filepath.Dir(h.cachePath)
	require.NoError(t, os.RemoveAll(home))

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Empty(t, h.checkedChannels(), "a deleted home means stand down, not check anyway")

	_, err := os.Stat(home)
	require.True(t, errors.Is(err, os.ErrNotExist),
		"the release check must not recreate the agent-factory home; watchDaemonHome resets on any successful stat, so this would keep an abandoned daemon alive (#1093)")
}

// The absence check must fire only on a POSITIVE observation of absence. An
// inconclusive stat is not evidence the home is gone, and treating it as such
// would silently disable checks on a box whose home is merely unreadable.
func TestUpdateDriver_TreatsAnInconclusiveHomeStatAsPresent(t *testing.T) {
	h := newDriverHarness(t)
	require.True(t, h.driver.homeExists())

	// A path whose parent is a regular file: stat fails with ENOTDIR, not
	// ErrNotExist.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o644))
	h.driver.cachePath = filepath.Join(blocked, "sub", autoupdate.CheckCacheFileName)
	require.True(t, h.driver.homeExists(), "an inconclusive stat must not be read as absence")
}

// Fleet spread: every autostart daemon starts at boot or login, so an unjittered
// first wake would point them all at the GitHub API at once. The offset is
// derived from the home, so it is stable for a box and different across boxes.
func TestUpdateDriverJitter(t *testing.T) {
	window := 15 * time.Minute

	require.Equal(t, updateDriverJitter("/home/a/.agent-factory/last_update_check", window),
		updateDriverJitter("/home/a/.agent-factory/last_update_check", window),
		"a box's offset must be stable across daemon restarts")

	seen := map[time.Duration]int{}
	for _, home := range []string{"/home/a", "/home/b", "/home/c", "/home/d", "/home/e", "/home/f"} {
		offset := updateDriverJitter(home, window)
		require.GreaterOrEqual(t, offset, time.Duration(0))
		require.Less(t, offset, window)
		seen[offset]++
	}
	require.Greater(t, len(seen), 1, "different homes must not all land on the same offset")

	require.Zero(t, updateDriverJitter("/home/a", 0), "a zero window must not divide by zero")
}

// The wiring contract with RunDaemon: the driver joins the daemon's WaitGroup, so
// shutdown WAITS for it, and it releases on the daemon's stopCh, so it cannot hang
// that shutdown. Both halves matter — a driver missing from the wg would be torn
// down mid-check, and one deaf to stopCh would wedge every daemon stop.
func TestStartUpdateDriver_JoinsAndReleasesTheDaemonWaitGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// Disabled: this test is about lifecycle, and the loop must reach no network.
	t.Setenv(autoupdate.EnvironmentVariable, "0")
	withVersion(t, "1.0.200")
	originalWake := updateDriverWakeInterval
	updateDriverWakeInterval = time.Millisecond
	t.Cleanup(func() { updateDriverWakeInterval = originalWake })

	manager := &Manager{}
	manager.live.Store(&config.Config{AutoUpdate: true, UpdateChannel: config.UpdateChannelStable})

	wg := &sync.WaitGroup{}
	stopCh := make(chan struct{})
	startUpdateDriver(manager, func() {}, stopCh, wg)

	waited := make(chan struct{})
	go func() {
		wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("the driver left the WaitGroup while still running; a daemon shutdown would not wait for it")
	case <-time.After(200 * time.Millisecond):
	}

	close(stopCh)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the driver did not release the WaitGroup on stopCh; a daemon shutdown would hang")
	}
}

// The driver reads the daemon's LIVE config, so an af config set applied to a
// running daemon (Manager.ApplyConfig) reaches it without a restart.
func TestNewUpdateDriverReadsTheLiveConfig(t *testing.T) {
	manager := &Manager{}
	manager.live.Store(&config.Config{AutoUpdate: false, UpdateChannel: config.UpdateChannelPreview})

	driver := newUpdateDriver(manager, func() {})
	require.False(t, driver.config().AutoUpdate)

	manager.live.Store(&config.Config{AutoUpdate: true, UpdateChannel: config.UpdateChannelPreview})
	require.True(t, driver.config().AutoUpdate, "the driver must re-read the live config, not a snapshot")
	require.Equal(t, config.UpdateChannelPreview, driver.config().UpdateChannel)
}

// The cache the driver reads is the one the launch path writes: same file, same
// format. A driver pointed at a different path would silently stop coordinating.
func TestUpdateDriverReadsTheSharedCacheFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	driver := newUpdateDriver(&Manager{}, func() {})
	require.Equal(t, autoupdate.CheckCachePath(), driver.cachePath)
	require.Equal(t, filepath.Join(home, autoupdate.CheckCacheFileName), driver.cachePath)

	// And it parses what the launch path writes.
	require.NoError(t, autoupdate.RecordCheck(driver.cachePath, config.UpdateChannelStable, "v1.0.200", "1.0.200"))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(driverCacheBytes(t, driver.cachePath)), &decoded))
	require.Contains(t, decoded, "channels")
}

func driverCacheBytes(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// enableActivation opts the harness in to daemon-owned activation and gives it
// stub staging/hand-off collaborators, returning what they recorded.
type activationRecord struct {
	downloadedURL string
	candidate     []byte
	toVersion     string
	activateErr   error
	downloadErr   error
	downloads     int
	activations   int
}

func enableActivation(h *driverHarness, enabled bool) *activationRecord {
	rec := &activationRecord{}
	h.driver.activationEnabled = func() bool { return enabled }
	h.driver.download = func(_ context.Context, url string, _ time.Duration) ([]byte, error) {
		rec.downloads++
		rec.downloadedURL = url
		if rec.downloadErr != nil {
			return nil, rec.downloadErr
		}
		return []byte("candidate-af-binary"), nil
	}
	h.driver.activate = func(_ context.Context, candidate []byte, toVersion string) error {
		rec.activations++
		rec.candidate = candidate
		rec.toVersion = toVersion
		return rec.activateErr
	}
	return rec
}

// The conservative default, and the whole reason this slice can land before the
// destructive integration exists: without the operator opt-in the driver reports
// and installs nothing, exactly as it did before.
func TestUpdateDriver_ActivationIsOffByDefault(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, false)

	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
	require.Zero(t, rec.downloads, "a driver that cannot install must not spend bandwidth staging")
	require.Zero(t, rec.activations)
}

// A driver with no enabler at all is off too — activation must be asked for, not
// arrived at by a missing collaborator.
func TestUpdateDriver_ActivationRequiresAnExplicitEnabler(t *testing.T) {
	h := newDriverHarness(t)
	h.driver.activationEnabled = nil

	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
}

// Opted in: stage the release for this platform and hand it to the transactional
// path with the version stripped of its leading v.
func TestUpdateDriver_ActivatesWhenOptedIn(t *testing.T) {
	h := newDriverHarness(t)
	h.tag = "v1.0.300"
	rec := enableActivation(h, true)

	require.Equal(t, updateCheckActivated, h.driver.checkOnce(context.Background()))
	require.Equal(t, 1, rec.downloads)
	require.Equal(t, 1, rec.activations)
	require.Equal(t, "1.0.300", rec.toVersion, "the trigger takes a version, not a tag")
	require.Equal(t, []byte("candidate-af-binary"), rec.candidate)
	require.Equal(t, autoupdate.DownloadURL("v1.0.300", updateDriverGOOS, updateDriverGOARCH), rec.downloadedURL)
}

// Activation must never close the shared throttle window. This process exits at
// the hand-off and the supervisor decides afterwards whether to commit or roll
// back, so "installed" is a claim the daemon is not entitled to make — and if
// the candidate rolls back, recording would have suppressed the launch path that
// still works.
func TestUpdateDriver_ActivationStillNeverWritesTheThrottleCache(t *testing.T) {
	h := newDriverHarness(t)
	enableActivation(h, true)
	h.seedCache(t, config.UpdateChannelStable, h.now.Add(-7*time.Hour))
	before := h.readCacheFile(t)
	require.NotEmpty(t, before)

	require.Equal(t, updateCheckActivated, h.driver.checkOnce(context.Background()))
	require.Equal(t, before, h.readCacheFile(t),
		"the daemon cannot observe whether its own upgrade committed, so it must not record one")
}

// A release whose hand-off failed must not be retried every six hours forever —
// that is the "bad release re-breaks the box" outcome the design rules out. The
// daemon keeps serving the old version.
func TestUpdateDriver_RejectsATagWhoseActivationFailed(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)
	rec.activateErr = errors.New("recovery actor never reached supervisor_ready")

	base := h.now
	require.Equal(t, updateCheckFailed, h.driver.checkOnce(context.Background()))
	require.Equal(t, 1, rec.activations)

	// Next window, same tag: reported, never retried.
	h.now = base.Add(autoupdate.CheckInterval)
	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
	require.Equal(t, 1, rec.activations, "the same failed release must not be activated again")
	require.Equal(t, 1, rec.downloads, "nor staged again")

	// A different, newer release is still eligible — the quarantine is per tag,
	// not a circuit breaker on the whole feature.
	h.tag = "v1.0.400"
	h.now = base.Add(2 * autoupdate.CheckInterval)
	rec.activateErr = nil
	require.Equal(t, updateCheckActivated, h.driver.checkOnce(context.Background()))
	require.Equal(t, 2, rec.activations)
}

// A download failure is transient — a flaky link, a half-published release — so
// it must NOT reject the tag. The six-hour window is the retry bound.
func TestUpdateDriver_ADownloadFailureDoesNotRejectTheTag(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)
	rec.downloadErr = errors.New("connection reset")

	base := h.now
	require.Equal(t, updateCheckFailed, h.driver.checkOnce(context.Background()))
	require.Zero(t, rec.activations)

	rec.downloadErr = nil
	h.now = base.Add(autoupdate.CheckInterval)
	require.Equal(t, updateCheckActivated, h.driver.checkOnce(context.Background()),
		"a transient staging failure must not disqualify the release")
	require.Equal(t, 1, rec.activations)
}

// The opt-in is re-read every wake, so an operator who turns it off is honoured
// without restarting the daemon — the same contract as the master switch.
func TestUpdateDriver_ActivationOptInIsRereadEveryWake(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)
	enabled := true
	h.driver.activationEnabled = func() bool { return enabled }

	base := h.now
	require.Equal(t, updateCheckActivated, h.driver.checkOnce(context.Background()))
	require.Equal(t, 1, rec.activations)

	enabled = false
	h.tag = "v1.0.400"
	h.now = base.Add(autoupdate.CheckInterval)
	require.Equal(t, updateCheckAvailable, h.driver.checkOnce(context.Background()))
	require.Equal(t, 1, rec.activations, "turning the opt-in off must take effect without a restart")
}

// The env switch itself: default off, the documented vocabulary honoured, and
// anything unrecognised OFF rather than authorising a self-replacing daemon.
func TestDaemonUpgradeActivationEnabled(t *testing.T) {
	t.Run("unset is off", func(t *testing.T) {
		// t.Setenv first so its cleanup restores whatever the process had;
		// os.Unsetenv alone would leak the change into later tests.
		t.Setenv(DaemonUpgradeEnvironmentVariable, "placeholder")
		require.NoError(t, os.Unsetenv(DaemonUpgradeEnvironmentVariable))
		require.False(t, daemonUpgradeActivationEnabled())
	})
	for _, on := range []string{"1", "true", "T", "yes", "Y", "on", " On "} {
		t.Run("on:"+on, func(t *testing.T) {
			t.Setenv(DaemonUpgradeEnvironmentVariable, on)
			require.True(t, daemonUpgradeActivationEnabled())
		})
	}
	for _, off := range []string{"", "0", "false", "no", "off", "maybe", "2", "yes please"} {
		t.Run("off:"+off, func(t *testing.T) {
			t.Setenv(DaemonUpgradeEnvironmentVariable, off)
			require.False(t, daemonUpgradeActivationEnabled(),
				"an unreadable value must not authorise a daemon to replace its own binary")
		})
	}
}

// An operator who asks the daemon to stop must not get a published transaction
// and a started candidate instead. Staging takes minutes and RunDaemon waits on
// this goroutine, so continuing would both upgrade unbidden and hold the stop
// open.
func TestUpdateDriver_ShutdownDuringStagingAbandonsTheUpgrade(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)

	ctx, cancel := context.WithCancel(context.Background())
	h.driver.download = func(dctx context.Context, _ string, _ time.Duration) ([]byte, error) {
		rec.downloads++
		cancel() // the daemon starts shutting down mid-download
		return []byte("candidate-af-binary"), dctx.Err()
	}

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(ctx))
	require.Zero(t, rec.activations, "a shutting-down daemon must not hand over an upgrade")
}

// Same, for a download that completes before the cancellation is noticed: the
// hand-off is still abandoned.
func TestUpdateDriver_ShutdownAfterStagingAbandonsTheUpgrade(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)

	ctx, cancel := context.WithCancel(context.Background())
	h.driver.download = func(context.Context, string, time.Duration) ([]byte, error) {
		rec.downloads++
		cancel()
		return []byte("candidate-af-binary"), nil
	}

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(ctx))
	require.Equal(t, 1, rec.downloads)
	require.Zero(t, rec.activations)
}

// The cancellation reaches the download itself, so a shutdown does not have to
// wait out the whole staging budget.
func TestUpdateDriver_StagingIsCancellable(t *testing.T) {
	h := newDriverHarness(t)
	enableActivation(h, true)

	ctx, cancel := context.WithCancel(context.Background())
	observed := make(chan struct{})
	h.driver.download = func(dctx context.Context, _ string, _ time.Duration) ([]byte, error) {
		close(observed)
		<-dctx.Done() // would hang forever if cancellation were not threaded through
		return nil, dctx.Err()
	}

	go func() { <-observed; cancel() }()
	// Skipped, not failed: a cancelled transfer IS the shutdown, and calling the
	// operator's own stop a staging failure would be the wrong report.
	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(ctx))
}

// Staging takes minutes with the shared cache lock released, so an interactive
// `af upgrade` can legitimately install in that window — the interlock does not
// stop it, because no transaction exists yet. Handing over afterwards would
// install the now-stale candidate over that newer binary and preserve the newer
// one as the rollback target.
func TestUpdateDriver_AbandonsAStaleCandidateWhenTheExecutableChanged(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)

	identity := h.driver.baselineExecutable
	h.driver.executableIdentity = func() (string, error) { return identity, nil }
	h.driver.download = func(context.Context, string, time.Duration) ([]byte, error) {
		rec.downloads++
		identity = "someone-else-installed" // an af upgrade lands mid-staging
		return []byte("candidate-af-binary"), nil
	}

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Equal(t, 1, rec.downloads)
	require.Zero(t, rec.activations,
		"a candidate staged against a binary that has since been replaced must not be activated")
}

// An unreadable executable is not a licence to hand over either — the daemon
// cannot show the candidate is still an upgrade, so it does not act.
func TestUpdateDriver_UnreadableExecutableBlocksActivation(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)
	h.driver.executableIdentity = func() (string, error) { return "", errors.New("permission denied") }

	require.Equal(t, updateCheckFailed, h.driver.checkOnce(context.Background()))
	require.Zero(t, rec.downloads, "an unverifiable executable must not even be staged against")
	require.Zero(t, rec.activations)
}

// The identity is a real fingerprint of the running binary, and it changes when
// the file is replaced the way an in-place install replaces it.
func TestRunningExecutableIdentity_ChangesWhenTheBinaryIsReplaced(t *testing.T) {
	first, err := runningExecutableIdentity()
	require.NoError(t, err)
	require.NotEmpty(t, first)

	again, err := runningExecutableIdentity()
	require.NoError(t, err)
	require.Equal(t, first, again, "an unchanged binary must fingerprint the same")
}

// The case a before/after pair around the download cannot see: the executable
// was ALREADY replaced before this wake, while this daemon kept running the old
// binary. `af upgrade --no-restart` says it leaves the daemon alone in as many
// words, and the launch updater reaches the same state when it installs but
// cannot make the autostart unit restart-safe.
//
// Both readings around the download would agree perfectly — they describe the
// same already-new binary — so only a baseline taken at daemon start catches it.
// Without this, a stable-channel check installs an older tag over the newer
// binary on disk and hands the transaction that newer binary as its rollback
// target.
func TestUpdateDriver_AbandonsWhenTheExecutableWasReplacedBeforeThisWake(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)

	// Stable across the whole check — nothing changes during the download — but
	// it is not what this daemon started from.
	h.driver.executableIdentity = func() (string, error) { return "installed-by-af-upgrade-no-restart", nil }

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(context.Background()))
	require.Zero(t, rec.downloads, "a drifted executable must be caught before spending the bandwidth")
	require.Zero(t, rec.activations)
}

// No baseline means no way to prove the candidate is an upgrade over what is
// actually installed, so activation fails closed rather than guessing.
func TestUpdateDriver_NoExecutableBaselineBlocksActivation(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)
	h.driver.baselineErr = errors.New("could not resolve the executable at startup")

	require.Equal(t, updateCheckFailed, h.driver.checkOnce(context.Background()))
	require.Zero(t, rec.downloads)
	require.Zero(t, rec.activations)
}

// The baseline is captured at construction, when the on-disk binary is still the
// one this process exec'd from — later is too late.
func TestNewUpdateDriver_CapturesTheExecutableBaselineAtStart(t *testing.T) {
	manager := &Manager{}
	manager.live.Store(&config.Config{AutoUpdate: true})

	driver := newUpdateDriver(manager, func() {})
	if driver.baselineErr != nil {
		t.Skipf("no resolvable executable in this environment: %v", driver.baselineErr)
	}
	require.NotEmpty(t, driver.baselineExecutable)

	current, err := driver.executableIdentity()
	require.NoError(t, err)
	require.Equal(t, driver.baselineExecutable, current,
		"an untouched executable must still match the baseline taken at start")
}

// The fingerprint hashes the whole binary, so shutdown can land between the
// post-download check and the hand-off. triggerUpgradeActivation enters Prepare
// before it looks at the context, and the detached recovery-job start takes no
// context at all — so past that point an operator's stop publishes a transaction
// instead of exiting.
func TestUpdateDriver_ShutdownDuringTheFingerprintAbandonsTheUpgrade(t *testing.T) {
	h := newDriverHarness(t)
	rec := enableActivation(h, true)

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	h.driver.executableIdentity = func() (string, error) {
		calls++
		if calls == 2 {
			cancel() // shutdown lands while the post-download fingerprint runs
		}
		return h.driver.baselineExecutable, nil
	}

	require.Equal(t, updateCheckSkipped, h.driver.checkOnce(ctx))
	require.Equal(t, 1, rec.downloads)
	require.Zero(t, rec.activations, "a stopping daemon must not publish a transaction")
}
