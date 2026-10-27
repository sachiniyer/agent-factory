package testresidue

import (
	"os"
	"path/filepath"
	"testing"
)

// The recognizer's package doc claims it "can't drift from what the harness
// creates." That claim is empty without a test that asks the harness's own
// os.MkdirTemp call sites what names they get back, and fails when one of them
// is not recognised. #4170's sandbox-HOME regression is exactly that drift: a
// later testguard change added the af-test-user-home- os.MkdirTemp without a
// Classify case, and af doctor silently exempted every sandbox-HOME dir from
// its test-residue count. These tests pin the agreement the doc promised.

// harnessPrefixSites is the closed, hand-maintained list of os.MkdirTemp("",
// <prefix>) call sites in internal/testguard that land a directory directly
// under the temp root. Every entry must carry a Classify case, or
// TestClassifyRecognisesEveryHarnessPrefix fails — which is what makes adding a
// new harness prefix without a recognizer row a CI failure instead of a silent
// #4170-class regression.
//
// Each site is anchored to the function that calls os.MkdirTemp so a future
// reader can follow the name from where it is made to where it is recognised.
var harnessPrefixSites = []struct {
	prefix string
	want   Kind
	site   string
}{
	{SandboxHomePrefix, SandboxHome,
		"testguard.go SandboxHome's AGENT_FACTORY_HOME (os.MkdirTemp of testresidue.SandboxHomePrefix)"},
	{SandboxUserHomePrefix, SandboxUserHome,
		"userhome.go sandboxUserHome's sandboxed HOME (called via testguard.go SandboxHome)"},
	{PackageTmuxPrefix, TmuxSocketDir,
		"testguard.go SandboxTmux's per-package TMUX_TMPDIR (os.MkdirTemp of testresidue.PackageTmuxPrefix)"},
	{TestTmuxPrefix, TmuxSocketDir,
		"testguard.go IsolateTmux's per-test TMUX_TMPDIR (os.MkdirTemp of testresidue.TestTmuxPrefix)"},
	// SocketTempDir's af-* dirs are per-test and t.TempDir-cleaned, so they are
	// deliberately NOT recognised; this row pins that decision, so a Classify
	// case for the bare "af-" prefix would fail here.
	{"af-", None,
		"testguard.go SocketTempDir's per-test socket dir (deliberately out of scope)"},
}

// TestClassifyRecognisesEveryHarnessPrefix asks every os.MkdirTemp("", <prefix>)
// site in internal/testguard what name it actually got back, and asserts
// Classify agrees. It fails the moment a harness prefix is not recognised —
// the #4170 invisibility regression for the sandbox-HOME family. Asserting the
// exact Kind (not just "not None") is what keeps the two af-test-home- /
// af-test-user-home- prefixes from collapsing into one kind, since a
// sandboxed HOME and an AGENT_FACTORY_HOME hold different content and a
// removal keys on that content.
func TestClassifyRecognisesEveryHarnessPrefix(t *testing.T) {
	root := t.TempDir()
	for _, s := range harnessPrefixSites {
		s := s
		t.Run(s.prefix, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				dir, err := os.MkdirTemp(root, s.prefix)
				if err != nil {
					t.Fatal(err)
				}
				name := filepath.Base(dir)
				got := Classify(name)
				if got != s.want {
					t.Errorf("Classify(%q from %s) = %v, want %v — the harness creates this "+
						"name directly under the temp dir; if doctor's test-residue check does "+
						"not see it, the dir is invisible to both doctor and the in-harness "+
						"reaper (#4170 invisibility regression)", name, s.site, got, s.want)
				}
				// MkdirTemp names are consumed entry by entry; clean up so the
				// 50-iteration loop does not pile thousands of dirs onto root.
				_ = os.Remove(dir)
			}
		})
	}
}
