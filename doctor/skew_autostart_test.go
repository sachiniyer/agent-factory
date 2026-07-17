package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// The autostart half of the skew suite (#1044): whose unit is it, what binary
// does it launch, and is anything actually supervising the daemon. Split out of
// skew_test.go to stay under the file-length limit (#1145); the shared helpers
// live there and are visible here because it is one package.

// A different path AND a different version: the unit respawns a binary that is
// not the one you run, so no restart can fix it. This is the real stranding.
func TestAutostartPath_DifferentBinaryAndVersion_Fails(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, Path: "/fake/unit.service", ExecPath: "/usr/local/bin/af"}
	}
	opts.selfBinary = func() (string, error) { return "/home/dev/.local/bin/af", nil }
	opts.binaryVersion = func(string) (string, error) { return "1.0.180", nil }

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart path")
	require.Equal(t, StatusFail, c.Status)
	require.Contains(t, c.Detail, "/usr/local/bin/af")
	require.Contains(t, c.Detail, "/home/dev/.local/bin/af")
	require.Contains(t, c.Detail, "1.0.180")
	require.Contains(t, c.Remediation, "af daemon install")
	require.True(t, c.Problem)
}

// The dev-box false positive this check must not have: running a binary you
// just built while the unit points at your installed af is normal, correct, and
// nothing to fix. The paths differ; the versions do not.
func TestAutostartPath_DifferentPathSameVersion_AdvisoryOnly(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: "/home/dev/.local/bin/af"}
	}
	opts.selfBinary = func() (string, error) { return "/tmp/go-build123/af", nil }
	opts.binaryVersion = func(string) (string, error) { return "1.0.192", nil }

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart path")
	require.Equal(t, StatusWarn, c.Status, "same version at two paths is a note, not a verdict")
	require.Contains(t, c.Detail, "nothing is skewed today")
	require.False(t, c.Problem, "a scratch build must not make a healthy box exit nonzero")
	require.Zero(t, report.UnresolvedCount())
}

// There is ONE autostart unit per user, and it bakes its AGENT_FACTORY_HOME at
// install time. So under AGENT_FACTORY_HOME=/tmp/sandbox, the developer's unit
// is still the only unit on the box — and it is not this home's. Every autostart
// row must then be silent rather than assert about somebody else's daemon: the
// same whose-home defect as #1916's P2 and #1950.
func TestAutostart_UnitServingAnotherHome_NotTreatedAsOurs(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	// A unit IS installed — it simply serves a different home.
	opts.autostartServesHome = func(string) (bool, bool, error) { return false, true, nil }
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: "/usr/local/bin/af"}
	}
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(), Active: daemon.AnswerYes()}
	}
	opts.selfBinary = func() (string, error) { return "/home/dev/.local/bin/af", nil }
	opts.binaryVersion = func(string) (string, error) { return "1.0.180", nil }

	report, err := Run(opts)
	require.NoError(t, err)

	require.False(t, hasCheck(report, "autostart path"),
		"another home's unit must not be compared against our binary")
	require.False(t, hasCheck(report, "autostart supervision"),
		"nor may its supervision be reported as ours")

	// The health row explains it instead of claiming autostart is installed here.
	c := findCheck(t, report, "autostart")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "different agent-factory home")
	require.False(t, c.Problem, "a sandbox home without autostart is a choice, not a fault")
	require.Zero(t, report.UnresolvedCount())
}

// A unit that serves this home is ours to report on, as before.
func TestAutostart_UnitServingThisHome_IsReported(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(), Active: daemon.AnswerYes()}
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart").Status)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart supervision").Status)
}

// One condition, one row. Both autostart checks ask whether the unit is ours, so
// a scope failure reported by the asker would print twice for a single cause —
// and checkDaemonHealth's "autostart" row already owns every one of these states.
func TestAutostart_ScopeFailure_ReportedExactlyOnce(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.autostartServesHome = func(string) (bool, bool, error) { return false, false, os.ErrPermission }
	// Both checks would speak if they could.
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: "/usr/local/bin/af"}
	}
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	rows := 0
	for _, c := range report.Checks {
		if strings.Contains(c.Detail, "autostart unit") && c.Status != StatusPass {
			rows++
		}
	}
	require.Equal(t, 1, rows,
		"one unreadable unit must produce one row, not one per check that asked")

	// And it is checkDaemonHealth's row that owns it.
	require.Equal(t, StatusWarn, findCheck(t, report, "autostart").Status)
	require.False(t, hasCheck(report, "autostart scope"), "the asker must not report on its own")
}

// Whether the unit is ours must be established, not assumed: if the question
// cannot be answered, say so rather than guessing either way.
func TestAutostart_ScopeUnreadable_SaysSo(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.autostartServesHome = func(string) (bool, bool, error) { return false, false, os.ErrPermission }

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "cannot read the installed autostart unit")
	require.True(t, c.Problem)
	require.False(t, hasCheck(report, "autostart path"), "an unestablished scope must not be asserted about")
}

// A unit pointing at something that is not a readable af binary (a deleted
// path, say) cannot start a daemon at all — worth acting on, but it is not
// evidence of version skew, so it warns rather than asserting one.
func TestAutostartPath_UnitBinaryUnidentifiable_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = "1.0.192"
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: "/gone/af"}
	}
	opts.selfBinary = func() (string, error) { return "/home/dev/.local/bin/af", nil }
	opts.binaryVersion = func(string) (string, error) { return "", errNoDaemon }

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart path")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "not a readable af binary")
	require.True(t, c.Problem, "a unit that cannot launch af is still a real problem")
}

// An unreleased client cannot be compared, so the path difference is unjudgeable.
func TestAutostartPath_DevClient_AdvisoryOnly(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	opts.Version = devVersion
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: "/home/dev/.local/bin/af"}
	}
	opts.selfBinary = func() (string, error) { return "/tmp/af", nil }
	opts.binaryVersion = func(string) (string, error) { return "1.0.192", nil }

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart path")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "cannot be judged")
	require.False(t, c.Problem)
}

func TestAutostartPath_Matching_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	// A real path on disk, so EvalSymlinks resolves identically on both sides
	// (on macOS /tmp is itself a symlink, which is exactly the false positive
	// resolvePath exists to avoid).
	bin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: bin}
	}
	opts.selfBinary = func() (string, error) { return bin, nil }

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart path").Status)
}

// A symlinked install is one binary, not two: resolving both sides is what
// keeps ~/.local/bin/af -> /nix/store/… from reading as a split brain.
func TestAutostartPath_SymlinkedInstall_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	dir := t.TempDir()
	real := filepath.Join(dir, "af-real")
	require.NoError(t, os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755))
	link := filepath.Join(dir, "af")
	require.NoError(t, os.Symlink(real, link))

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{Supported: true, Exists: true, ExecPath: link}
	}
	opts.selfBinary = func() (string, error) { return real, nil }

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart path").Status,
		"a symlink to the same binary is not a path mismatch")
}

func TestAutostartPath_NoUnitInstalled_NoRow(t *testing.T) {
	testguard.IsolateTmux(t)

	report, err := Run(testOptions(t, false))
	require.NoError(t, err)
	require.False(t, hasCheck(report, "autostart path"), "no unit means nothing to compare")
}

// An installed-but-unreadable unit must be reported, never skipped. A
// diagnostic that silently drops a check when it hits a permissions error tells
// the user their machine is fine when it is not — the worst thing it can do.
func TestAutostartPath_UnitPresentButUnreadable_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartUnit = func() daemon.AutostartUnitInfo {
		return daemon.AutostartUnitInfo{
			Supported: true, Exists: true, Path: "/etc/systemd/user/af.service",
			Err: os.ErrPermission,
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart path")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "cannot be read")
	require.Contains(t, c.Detail, "/etc/systemd/user/af.service")
	require.True(t, c.Problem, "an unreadable unit is not a working one")
}

func TestAutostartSupervision_UnitUnreadableAndInactive_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true, Active: daemon.AnswerNo(), Err: os.ErrPermission,
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "cannot be read")
	require.True(t, c.Problem)
}

// The macOS domain mismatch: the agent is loaded, so it looks supervised, but
// `launchctl kickstart -k gui/<uid>/…` restarts sail past it and the old daemon
// lives on.
func TestAutostartSupervision_LoadedInWrongDomain_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(),
			Domain: "gui/501/com.agent-factory.daemon", LoadedElsewhere: daemon.AnswerYes(),
			Detail: "loaded outside gui/501/com.agent-factory.daemon",
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "gui/501/com.agent-factory.daemon")
	require.Contains(t, c.Remediation, "af daemon install")
	require.True(t, c.Problem)
}

func TestAutostartSupervision_UnitPresentButInactive_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(), Active: daemon.AnswerNo(),
			Detail: "is-enabled=enabled is-active=inactive",
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "not running it")
	require.True(t, c.Problem)
}

// Loaded is not running: the agent is installed and launchd knows it, so
// everything looks configured while no daemon is actually up. Reporting PASS
// here is a false all-clear on the platform where this was hit.
func TestAutostartSupervision_LoadedButNotRunning_Warns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(),
			Loaded: daemon.AnswerYes(), Active: daemon.AnswerNo(),
			Domain: "gui/501/com.agent-factory.daemon",
			Detail: "loaded in gui/501/com.agent-factory.daemon but no daemon process is running",
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status, "a loaded-but-dead agent must never read as PASS")
	require.Contains(t, c.Detail, "no daemon process is running")
	require.True(t, c.Problem)
}

// "I asked and it said no" and "I could not ask" are different facts. When the
// service manager cannot be queried, doctor must say unknown WITH the cause —
// never fold it into "not running", which would tell the user their autostart is
// off when we have no idea. Same disease this PR detects, one level in.
func TestAutostartSupervision_ProbeFailed_RendersUnknownNotInactive(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true,
			// Everything unknown, with a cause — what a missing systemctl or a
			// dead user bus produces.
			Enabled: daemon.Undetermined(errors.New("could not query systemd (is-enabled): exec: \"systemctl\": executable file not found in $PATH")),
			Active:  daemon.Undetermined(errors.New("could not query systemd (is-active): exec: \"systemctl\": executable file not found in $PATH")),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "unknown", "the row must say it does not know")
	require.Contains(t, c.Detail, "could not query systemd", "and must carry the cause")
	require.NotContains(t, c.Detail, "not running it",
		"an inability to ask must never be rendered as a negative answer")
	require.False(t, c.Problem, "we cannot assert a problem we could not observe")
}

// A probe that answers nothing definitive, without an error, is still unknown.
func TestAutostartSupervision_NoDefiniteState_RendersUnknown(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Detail: "is-enabled=unknown is-active=unknown"}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "unknown")
	require.False(t, c.Problem)
}

func TestAutostartSupervision_EnabledAndActive_Passes(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Enabled: daemon.AnswerYes(), Active: daemon.AnswerYes()}
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Equal(t, StatusPass, findCheck(t, report, "autostart supervision").Status)
}

// The NOT-FOUND answer must reach the user as its own diagnosis with its own
// remedy — it is the one the two-valued probe threw away.
//
// The unit file is installed but the service manager has no record of it, which
// means it was never loaded. Reported as "inactive" this sent users to reinstall
// a unit that was already there; reported as "unknown" it told them nothing. The
// fix is `systemctl --user daemon-reload`, and only this outcome can say so.
func TestAutostartSupervision_UnitUnknownToManager_HasItsOwnRemedy(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true,
			Enabled: daemon.AnswerNotFound(),
			Active:  daemon.AnswerNotFound(),
			Detail:  "is-enabled=not-found is-active=not-found",
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "no record of it")
	require.Contains(t, c.Remediation, "daemon-reload",
		"the fix for a unit systemd never loaded is a reload, not a reinstall")
	require.NotContains(t, c.Detail, "not running it",
		"not-found is not the same fact as inactive")
	require.True(t, c.Problem)
}

// A daemon that IS running while the manager has no record of the unit: it runs
// now, but nothing will start it at login.
func TestAutostartSupervision_RunningButUnitUnknown_StillWarns(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true,
			Active:  daemon.AnswerYes(),
			Enabled: daemon.AnswerNotFound(),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "won't start at login")
	require.True(t, c.Problem)
}

// The user-visible verdict is what lied, so that is what this asserts.
//
// `systemctl is-enabled` exits 0 for "static", and doctor said "unit is enabled
// and running" — a PASS. The user's daemon does not come back after a reboot and
// doctor told them everything was fine. That is worse than any false negative
// this PR has fixed: a false negative sends someone to poke a working system,
// but this one leaves them broken and unwarned, which is the failure #1920
// exists to prevent.
func TestAutostartSupervision_ExitZeroButNotEnabled_IsNotHealthy(t *testing.T) {
	testguard.IsolateTmux(t)

	for _, word := range []string{"static", "indirect", "enabled-runtime", "generated", "transient"} {
		t.Run(word, func(t *testing.T) {
			opts := testOptions(t, false)
			ourAutostartUnit(&opts)
			opts.autostartSupervision = func() daemon.SupervisionInfo {
				// What the classifier now yields for these: running, but not
				// enabled to start at login.
				return daemon.SupervisionInfo{
					Supported: true, UnitPresent: true,
					Active:  daemon.AnswerYes(),
					Enabled: daemon.AnswerNo(),
					Detail:  "is-enabled=" + word + " is-active=active",
				}
			}

			report, err := Run(opts)
			require.NoError(t, err)

			c := findCheck(t, report, "autostart supervision")
			require.NotEqual(t, StatusPass, c.Status,
				"is-enabled=%s exits 0 but nothing starts af at login — this must never read as healthy", word)
			require.Contains(t, c.Detail, "won't start at login")
			require.Contains(t, c.Detail, word, "and the user must see WHICH state systemd reported")
			require.True(t, c.Problem, "an autostart that will not autostart is a real problem")
		})
	}
}

// The alias case, end to end: unknowable enablement must not read as healthy
// either — but it is advisory, because we did not observe a fault.
func TestAutostartSupervision_AliasEnablement_IsUnknownNotHealthy(t *testing.T) {
	testguard.IsolateTmux(t)

	opts := testOptions(t, false)
	ourAutostartUnit(&opts)
	opts.autostartSupervision = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{
			Supported: true, UnitPresent: true,
			Active:  daemon.AnswerYes(),
			Enabled: daemon.Undetermined(errors.New("the unit name is an alias for another unit")),
		}
	}

	report, err := Run(opts)
	require.NoError(t, err)

	c := findCheck(t, report, "autostart supervision")
	require.Equal(t, StatusWarn, c.Status)
	require.Contains(t, c.Detail, "unknown")
	require.False(t, c.Problem, "we did not observe a fault, so we do not assert one")
}
