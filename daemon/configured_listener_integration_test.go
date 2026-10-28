package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These tests pin the listener-half invariant the live-ApplyConfig rebind broke:
// after a successful `af config set network.listen_addr` (or preview_listen_addr)
// that crosses the configured↔unconfigured boundary post-boot, the lifecycle's
// CONFIGURED half (TCPConfigured/TCPListenAddr and the preview twins) must track
// the address the operator just applied, not stay frozen at the boot-time value.
// `af daemon status` keys its render on TCPConfigured first (commands/daemoncmd.go),
// and /v1/health serialises the same field, so a stale configured half renders a
// posture that contradicts reality for the rest of the boot.
//
// They drive the PRODUCTION apply path end-to-end: config.SetGlobalConfigValue
// writes the new key to the on-disk config under a throwaway AGENT_FACTORY_HOME,
// and m.ApplyConfig() reloads it from disk (config.LoadConfig), swaps m.live, and
// calls m.webListeners.reconcile — the exact path an operator's `af config set`
// triggers. No hand-rolled m.live.Store or direct wl.reconcile call.

// managerWithWebListeners builds a manager whose live config is cfg and wires its
// webListeners so ApplyConfig's reconcile path runs the real bind/teardown. It
// performs the initial reconcile so the boot-time bound state matches the boot
// config. Unlike boundWebListeners (listener_reload_test.go) it does NOT require a
// bound control address, so a boot in the network.listen_addr="" opt-out state is
// representable. Cleanup closes the listeners.
func managerWithWebListeners(t *testing.T, cfg *config.Config) (*Manager, *webListeners) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	t.Cleanup(func() { _ = wl.close() })
	return m, wl
}

// applyListenAddr writes network.listen_addr to the on-disk config (under the
// manager's temp home) and applies it live through the production ApplyConfig
// path, failing the test if the apply itself errors or the listener key is
// reported as a failed rebind.
func applyListenAddr(t *testing.T, m *Manager, value string) {
	t.Helper()
	_, err := config.SetGlobalConfigValue("listen_addr", value)
	require.NoError(t, err)
	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys,
		"a successful live apply must not report the listener key as a failed rebind")
}

// applyPreviewListenAddr is applyListenAddr for network.preview_listen_addr.
func applyPreviewListenAddr(t *testing.T, m *Manager, value string) {
	t.Helper()
	_, err := config.SetGlobalConfigValue("preview_listen_addr", value)
	require.NoError(t, err)
	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Empty(t, result.FailedListenerKeys,
		"a successful live apply must not report the preview listener key as a failed rebind")
}

// TestConfiguredListenerFieldsTrackApplyConfigRebind is the enable direction of
// the bug: a daemon booted in the network.listen_addr="" opt-out state has
// TCPConfigured=false at construction. A successful live ApplyConfig that enables
// network.listen_addr must re-sync the configured half so TCPConfigured becomes
// true and TCPListenAddr is the applied address — not frozen at the boot-time
// empty value while the bound half (TCPBound/TCPBoundAddr) already tracks the new
// listener. RED before the fix: TCPConfigured stayed false while TCPBound was true.
func TestConfiguredListenerFieldsTrackApplyConfigRebind(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "" // boot in the opt-out state
	m, _ := managerWithWebListeners(t, cfg)

	snap := m.lifecycle.snapshot().listeners
	require.False(t, snap.TCPConfigured, "a boot with listen_addr empty must start unconfigured")
	require.False(t, snap.TCPBound)

	applyListenAddr(t, m, "127.0.0.1:0")

	snap = m.lifecycle.snapshot().listeners
	assert.True(t, snap.TCPConfigured,
		"after enabling network.listen_addr via ApplyConfig, TCPConfigured must be true")
	require.True(t, snap.TCPBound,
		"the bound half tracks the rebind regardless — this is the half that already worked")
	require.Equal(t, "127.0.0.1:0", snap.TCPListenAddr,
		"TCPListenAddr must be the configured address the operator applied")
	require.NotEmpty(t, snap.TCPBoundAddr,
		"TCPBoundAddr is the kernel-resolved concrete address — distinct from the configured one")
}

// TestConfiguredListenerFieldsClearedAfterApplyConfigDisable is the disable
// direction — the realistic common one, since the shipped default is a populated
// listen_addr: a daemon booted WITH network.listen_addr bound has
// TCPConfigured=true. A successful live ApplyConfig that disables it
// (network.listen_addr="") must re-sync the configured half so TCPConfigured
// becomes false and TCPListenAddr is cleared — not frozen at the boot-time
// configured value while the bound half (TCPBound) is already torn down. RED
// before the fix: TCPConfigured stayed true while TCPBound was false.
func TestConfiguredListenerFieldsClearedAfterApplyConfigDisable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0" // boot configured (the populated-default direction)
	m, _ := managerWithWebListeners(t, cfg)

	snap := m.lifecycle.snapshot().listeners
	require.True(t, snap.TCPConfigured, "a boot with listen_addr set must start configured")
	require.True(t, snap.TCPBound)
	require.Equal(t, "127.0.0.1:0", snap.TCPListenAddr)

	applyListenAddr(t, m, "")

	snap = m.lifecycle.snapshot().listeners
	assert.False(t, snap.TCPConfigured,
		"after disabling network.listen_addr via ApplyConfig, TCPConfigured must be false")
	require.False(t, snap.TCPBound,
		"the bound half is torn down regardless — this is the half that already worked")
	require.Empty(t, snap.TCPListenAddr,
		"TCPListenAddr must be cleared when the operator opts out")
	require.Empty(t, snap.TCPBoundAddr)
}

// TestPreviewConfiguredFieldsTrackApplyConfigRebind is the preview-listener twin
// of the enable direction (network.preview_listen_addr, #1856). The configured
// half is symmetric to the control listener and has the same bug; the same fix
// re-syncs it on a successful preview rebind.
func TestPreviewConfiguredFieldsTrackApplyConfigRebind(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PreviewListenAddr = "" // disabled by default
	m, _ := managerWithWebListeners(t, cfg)

	snap := m.lifecycle.snapshot().listeners
	require.False(t, snap.PreviewConfigured)
	require.False(t, snap.PreviewBound)

	applyPreviewListenAddr(t, m, "127.0.0.1:0")

	snap = m.lifecycle.snapshot().listeners
	assert.True(t, snap.PreviewConfigured,
		"after enabling network.preview_listen_addr via ApplyConfig, PreviewConfigured must be true")
	require.True(t, snap.PreviewBound)
	require.Equal(t, "127.0.0.1:0", snap.PreviewListenAddr)
	require.NotEmpty(t, snap.PreviewBoundAddr)
}

// TestPreviewConfiguredFieldsClearedAfterApplyConfigDisable is the preview-listener
// twin of the disable direction. Disabling a configured preview listener must
// clear the configured half, not leave it frozen at the boot-time value.
func TestPreviewConfiguredFieldsClearedAfterApplyConfigDisable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PreviewListenAddr = "127.0.0.1:0"
	m, _ := managerWithWebListeners(t, cfg)

	snap := m.lifecycle.snapshot().listeners
	require.True(t, snap.PreviewConfigured)
	require.True(t, snap.PreviewBound)

	applyPreviewListenAddr(t, m, "")

	snap = m.lifecycle.snapshot().listeners
	assert.False(t, snap.PreviewConfigured,
		"after disabling network.preview_listen_addr via ApplyConfig, PreviewConfigured must be false")
	require.False(t, snap.PreviewBound)
	require.Empty(t, snap.PreviewListenAddr)
	require.Empty(t, snap.PreviewBoundAddr)
}

// TestConfiguredFieldsUnchangedAfterUnexpectedListenerDeath pins the SCOPE of the
// fix: the unexpected-listener-death closure in bindWebLocked clears the BOUND half
// only, and must NOT touch the configured half. network.listen_addr is still set
// when a listener dies on its own (no config change), so the operator's configured
// address must remain so `af daemon status` renders "<addr> (not bound)" (the
// default branch) rather than "disabled". A fix that re-synced the configured half
// from the death path would hide the configured address an operator can still
// rebind onto. This test passes both before and after the fix; it guards the scope.
func TestConfiguredFieldsUnchangedAfterUnexpectedListenerDeath(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl := managerWithWebListeners(t, cfg)

	snap := m.lifecycle.snapshot().listeners
	require.True(t, snap.TCPConfigured)
	configuredAddr := snap.TCPListenAddr
	require.Equal(t, "127.0.0.1:0", configuredAddr)

	// Fail the bound listener directly, simulating an unexpected death — no config
	// change. The done watcher clears the bound half on its own generation.
	wl.mu.Lock()
	handle := wl.webHandle
	wl.mu.Unlock()
	require.NotNil(t, handle)
	require.NoError(t, handle.close())
	require.Eventually(t, func() bool { return !m.lifecycle.snapshot().listeners.TCPBound },
		time.Second, 10*time.Millisecond, "the done watcher must observe listener death and clear the bound half")

	snap = m.lifecycle.snapshot().listeners
	require.False(t, snap.TCPBound, "the bound half is cleared when the listener dies")
	require.True(t, snap.TCPConfigured,
		"the configured half must remain — network.listen_addr is still set; the operator can rebind")
	require.Equal(t, "127.0.0.1:0", snap.TCPListenAddr,
		"the configured address is unchanged after an unexpected death")
}
