package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testresidue"
)

// The SandboxUserHome content rule in readResidueShape is the one residue kind
// whose harness never writes an owner stamp (userhome.sandboxUserHome writes
// only .zshrc, the marker and optionally .gitconfig). Before the kind guard on
// the isOwnerStamp branch, a foreign file literally named owner or owner.tmp
// inside a leaked af-test-user-home-* was misfiled under shape.stamps instead of
// the default branch that flips shape.ok=false — so the foreign file was silently
// omitted from the report and --fix deleted it, violating the check's invariant
// that content the harness did not leave behind is never licensed for removal.
//
// notes.txt is the control: a foreign name that does not collide with the stamp
// constants, so it was always correctly caught by the default branch.

func TestSandboxUserHomeWithForeignOwnerFileIsKept(t *testing.T) {
	for _, name := range []string{
		testresidue.OwnerStampFile,     // "owner" — the bug
		testresidue.OwnerStampTempFile, // "owner.tmp" — the rename target
		"notes.txt",                    // control: known to be caught
	} {
		t.Run(name, func(t *testing.T) {
			root := socketTempHome(t)
			stubOpenFiles(t, true, nil)
			dir := sandboxUserHomeResidue(t, root, testresidue.SandboxUserHomePrefix+"12500")
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("data\n"), 0o600))
			ageTree(t, dir, filepath.Join(dir, name))

			report, err := Run(residueOptions(t, root, true)) // --fix run
			require.NoError(t, err)
			f := residueFinding(t, report, dir)
			require.False(t, f.Actionable,
				"a foreign %q file in a sandboxed HOME must not authorise removal", name)
			require.FileExists(t, filepath.Join(dir, name),
				"the foreign %q file must not be deleted", name)
			require.Contains(t, f.Detail, name+", which the test harness does not leave behind")
			require.Empty(t, f.FixAction, "a kept directory must not carry a removal action")
			require.DirExists(t, dir)
		})
	}
}
