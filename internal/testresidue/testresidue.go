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
	// PackageTmuxPrefix is testguard.SandboxTmux's TMUX_TMPDIR, one per package
	// run.
	PackageTmuxPrefix = "af-tmux-pkg-"
	// TestTmuxPrefix is testguard.IsolateTmux's TMUX_TMPDIR, one per test.
	TestTmuxPrefix = "af-tmux-"

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
func Classify(name string) Kind {
	switch {
	case numberAfter(name, SandboxHomePrefix):
		return SandboxHome
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
