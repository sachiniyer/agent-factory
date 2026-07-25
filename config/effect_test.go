package config

import (
	"strings"
	"testing"
)

// TestEveryManifestKeyHasAnEffectClass fails the moment a config key is added
// without deciding WHEN its change takes effect (#2480). An unclassified key falls
// to EffectUnknown, whose notice is a bare "Saved." — the vague answer this
// feature exists to replace with a per-key one.
func TestEveryManifestKeyHasAnEffectClass(t *testing.T) {
	for _, e := range Manifest() {
		if KeyEffectClass(e.Key) == EffectUnknown {
			t.Errorf("config key %q has no EffectClass — classify it in keyEffectClasses", e.Key)
		}
	}
}

// TestEffectNoticeIsPerKeyAndHonest pins the three contracts the notice must keep,
// each of which the old single canned sentence broke:
//   - an applied-live key a running daemon accepted says it is live NOW;
//   - a next-daemon-start key says exactly that, and not that it is live now;
//   - a client-side key points at the next af launch and NEVER mentions a daemon,
//     because none read it.
func TestEffectNoticeIsPerKeyAndHonest(t *testing.T) {
	applied := EffectNotice("default_program", true)
	if !strings.Contains(applied, "using the new value now") {
		t.Errorf("applied-live notice should say it is live now, got %q", applied)
	}

	pending := EffectNotice("branch_prefix", true)
	if !strings.Contains(pending, "next daemon start") {
		t.Errorf("next-daemon-start notice should defer to the next daemon start, got %q", pending)
	}
	if strings.Contains(pending, "using the new value now") {
		t.Errorf("next-daemon-start notice must not claim it is live now, got %q", pending)
	}

	client := EffectNotice("update_channel", true)
	if !strings.Contains(client, "launch af") {
		t.Errorf("client-side notice should point at the next af launch, got %q", client)
	}
	if strings.Contains(client, "daemon") {
		t.Errorf("client-side notice must not mention a daemon — none reads it — got %q", client)
	}
}

// TestEffectNoticeDowngradesAppliedLiveWithoutADaemon: an applied-live key still
// waits for the next daemon start when no daemon was running to apply it, so the
// CLI on a box with no daemon does not claim a change is live.
func TestEffectNoticeDowngradesAppliedLiveWithoutADaemon(t *testing.T) {
	n := EffectNotice("default_program", false)
	if strings.Contains(n, "using the new value now") {
		t.Errorf("with no daemon, applied-live must not claim it is live now, got %q", n)
	}
	if !strings.Contains(n, "next daemon start") {
		t.Errorf("with no daemon, applied-live should defer to the next daemon start, got %q", n)
	}
}

// TestKeyEffectClassClassifiesDottedLeavesByBase: a dynamic family leaf
// (program_overrides.claude) inherits its base key's class, since the daemon
// applies the whole map.
func TestKeyEffectClassClassifiesDottedLeavesByBase(t *testing.T) {
	if KeyEffectClass("program_overrides.claude") != EffectAppliedLive {
		t.Errorf("a dotted leaf should inherit its base key's class")
	}
}
