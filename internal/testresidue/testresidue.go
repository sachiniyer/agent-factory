// Package testresidue names the directories af's own test harness creates
// directly under the temp dir, so the harness that creates them and `af doctor`,
// which has to recognise them after a killed test run left them behind, read
// one definition (#4170).
//
// Before this package the names lived only as literals inside
// internal/testguard, and doctor could not see a single one of them: its temp
// home sweep identifies a home by marker files, and these directories hold one
// marker (a SandboxHome's agent-factory.log) or none (an empty tmux socket dir).
//
// It is a leaf on purpose. internal/testguard imports it and must stay clear of
// the config import cycle; doctor imports it without linking package testing
// into the af binary, which importing testguard itself would do.
package testresidue

import "strings"

const (
	// SandboxHomePrefix is testguard.SandboxHome's AGENT_FACTORY_HOME, one per
	// package run. What it reliably holds is that package's agent-factory.log.
	SandboxHomePrefix = "af-test-home-"
	// SandboxUserHomePrefix is testguard.SandboxHome's sandboxed $HOME, the
	// sibling temp dir userhome.sandboxUserHome points HOME at, one per package
	// run. It is NOT an AGENT_FACTORY_HOME: it holds a .zshrc, the
	// SandboxUserHomeMarker, and optionally a .gitconfig the harness seeds, and
	// no owner stamp — so the in-harness reaper cannot attribute it, and only
	// af doctor's content-based removal can clear it (#4170 sandbox-HOME
	// regression).
	SandboxUserHomePrefix = "af-test-user-home-"
	// PackageTmuxPrefix is testguard.SandboxTmux's TMUX_TMPDIR, one per package
	// run.
	PackageTmuxPrefix = "af-tmux-pkg-"
	// TestTmuxPrefix is testguard.IsolateTmux's TMUX_TMPDIR, one per test.
	TestTmuxPrefix = "af-tmux-"

	// SandboxUserHomeMarker is the file userhome.sandboxUserHome writes into the
	// sandboxed HOME, so a child test binary can tell it runs inside one. It is
	// also one of the leaves doctor's content rule keys on to authorise a
	// removal — the one file a real home never holds, since only a fresh
	// MkdirTemp dir ever receives it. Kept here, beside the prefix, so the
	// harness that writes it and doctor that reads it share one definition.
	SandboxUserHomeMarker = ".af-testguard-sandbox-home"

	// OwnerStampFile is the file testguard writes inside every one of these
	// directories, of either kind, naming the test binary that created it, so
	// a later test binary can reap the directory once that owner is provably
	// dead (#4468).
	OwnerStampFile = "owner"
	// OwnerStampTempFile is the stamp before its rename into place. A binary
	// killed between the write and the rename leaves it behind.
	OwnerStampTempFile = OwnerStampFile + ".tmp"
)

// Kind is which harness directory a name belongs to.
type Kind int

const (
	// None is a name the harness does not create.
	None Kind = iota
	// SandboxHome is a package run's sandboxed AGENT_FACTORY_HOME.
	SandboxHome
	// SandboxUserHome is a package run's sandboxed $HOME. The harness writes no
	// owner stamp into it (only SandboxHome does), so it is distinct from
	// SandboxHome: orphansweep cannot reap it and doctor's content rule, not an
	// owner stamp, decides it.
	SandboxUserHome
	// TmuxSocketDir is a private tmux server's TMUX_TMPDIR, per package or per
	// test. Both hold the same thing — tmux-<uid>/ and its sockets — so they are
	// one kind, and it is the one that can hold a tmux server worth stopping.
	TmuxSocketDir
)

// Classify reports which harness directory name is, or None.
//
// The match is exact rather than a prefix test, because the harness creates
// these with os.MkdirTemp, which appends a decimal random suffix and nothing
// else. Requiring that suffix is what keeps a name like af-tmux-pkg-1 from
// reading as an IsolateTmux dir ("pkg-1" is not a number), and keeps anything
// merely starting with af-tmux- out altogether.
//
// SandboxHomePrefix ("af-test-home-") and SandboxUserHomePrefix
// ("af-test-user-home-") diverge at the ninth character ('h' vs 'u'), so
// neither is a prefix of the other and numberAfter's CutPrefix keeps them
// disjoint regardless of the order of cases here: an af-test-home-<n> name
// never reads as a sandboxed HOME, and an af-test-user-home-<n> name never
// reads as an AGENT_FACTORY_HOME.
func Classify(name string) Kind {
	switch {
	case numberAfter(name, SandboxHomePrefix):
		return SandboxHome
	case numberAfter(name, SandboxUserHomePrefix):
		return SandboxUserHome
	case numberAfter(name, PackageTmuxPrefix), numberAfter(name, TestTmuxPrefix):
		return TmuxSocketDir
	default:
		return None
	}
}

func numberAfter(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
