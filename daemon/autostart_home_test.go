package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAutostartUnitHomeRoundTrip pins the unit renderers and their parsers
// together. AutostartUnitServesHome decides whether `af reset` may stop the
// supervised daemon, and it decides it by READING BACK the AGENT_FACTORY_HOME
// that InstallAutostart wrote. Those are two independent pieces of escaping
// (systemd's quote-aware Environment= grammar, launchd's XML), so a renderer
// change that the parser does not follow does not fail loudly — it silently
// reports the wrong home, and the gate either stops a daemon it must not touch
// or fails to stop the one it must.
//
// The awkward values are the point: a spaced install path (#1214), a '%'
// systemd specifier, and quote/backslash characters are exactly what the
// escaping exists for.
func TestAutostartUnitHomeRoundTrip(t *testing.T) {
	homes := []string{
		"/home/u/.agent-factory",
		"/tmp/sandbox-home",
		"/home/John Smith/af home",
		`/tmp/pct%home`,
		`/tmp/quote"home`,
		`/tmp/back\slash`,
	}
	for _, home := range homes {
		t.Run(home, func(t *testing.T) {
			unit := systemdAutostartUnit("/usr/local/bin/af", "/usr/bin:/bin", "/bin/zsh", home)
			got, found := systemdUnitEnvValue(unit, "AGENT_FACTORY_HOME")
			if !found {
				t.Fatalf("systemd unit for home %q exposes no AGENT_FACTORY_HOME:\n%s", home, unit)
			}
			if got != home {
				t.Errorf("systemd round trip = %q, want %q\nunit:\n%s", got, home, unit)
			}

			plist := launchdAutostartPlist("/usr/local/bin/af", "/usr/bin:/bin", "/bin/zsh", home, "/tmp/af.log")
			got, found = launchdPlistEnvValue(plist, "AGENT_FACTORY_HOME")
			if !found {
				t.Fatalf("launchd plist for home %q exposes no AGENT_FACTORY_HOME:\n%s", home, plist)
			}
			if got != home {
				t.Errorf("launchd round trip = %q, want %q", got, home)
			}
		})
	}
}

// TestAutostartUnitHome_AbsentMeansDefault: a unit installed WITHOUT
// AGENT_FACTORY_HOME serves the DEFAULT home. Reporting that as "no home found"
// and treating it as unknown would make the gate skip the ordinary supervised
// daemon — the one a normal `af reset` is supposed to pause.
func TestAutostartUnitHome_AbsentMeansDefault(t *testing.T) {
	unit := systemdAutostartUnit("/usr/local/bin/af", "/usr/bin", "/bin/zsh", "")
	if v, found := systemdUnitEnvValue(unit, "AGENT_FACTORY_HOME"); found {
		t.Errorf("unit installed with no AGENT_FACTORY_HOME exposes %q; want absent", v)
	}
	// PATH must still round-trip — the absence must be specific to the home, not
	// a parser that cannot see Environment= lines at all.
	if v, found := systemdUnitEnvValue(unit, "PATH"); !found || v != "/usr/bin" {
		t.Errorf("PATH round trip = %q found=%v, want /usr/bin", v, found)
	}
}

// TestAutostartUnitServesHome_GatesOnHome is the daemon-side lock for the #1916
// P2. A unit installed for one home must not be reported as serving another —
// that report is what lets `af reset` stop a daemon it was never asked to touch.
func TestAutostartUnitServesHome_GatesOnHome(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	realHome := t.TempDir()
	sandbox := t.TempDir()

	unit := systemdAutostartUnit("/usr/local/bin/af", "/usr/bin", "/bin/zsh", realHome)
	if err := os.WriteFile(filepath.Join(dir, autostartUnitName), []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}

	// Its own home: serves.
	serves, installed, err := AutostartUnitServesHome(realHome)
	if err != nil {
		t.Fatalf("AutostartUnitServesHome(realHome): %v", err)
	}
	if !installed || !serves {
		t.Errorf("unit for its OWN home: serves=%v installed=%v, want both true", serves, installed)
	}

	// A different home: installed, but must NOT be reported as serving it.
	serves, installed, err = AutostartUnitServesHome(sandbox)
	if err != nil {
		t.Fatalf("AutostartUnitServesHome(sandbox): %v", err)
	}
	if !installed {
		t.Error("installed = false; the unit file exists")
	}
	if serves {
		t.Error("a unit installed for a DIFFERENT AF home reported as serving the sandbox — " +
			"this is what lets a sandbox reset stop the real daemon (#1916 P2)")
	}
}

// TestAutostartUnitServesHome_NoUnit: no unit file means nothing to gate on, and
// crucially not an error — a machine that never ran `af daemon install` is the
// common case.
func TestAutostartUnitServesHome_NoUnit(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	serves, installed, err := AutostartUnitServesHome(t.TempDir())
	if err != nil {
		t.Fatalf("AutostartUnitServesHome: %v", err)
	}
	if serves || installed {
		t.Errorf("no unit file: serves=%v installed=%v, want both false", serves, installed)
	}
}
