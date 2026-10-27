package testresidue

import (
	"os"
	"path/filepath"
	"testing"
)

// The recognizer is only as good as its agreement with the names the harness
// actually gets back from os.MkdirTemp, so it is checked against real ones
// rather than against spellings written out by hand.
func TestClassifyRecognisesWhatMkdirTempReturns(t *testing.T) {
	root := t.TempDir()
	for prefix, want := range map[string]Kind{
		SandboxHomePrefix: SandboxHome,
		PackageTmuxPrefix: TmuxSocketDir,
		TestTmuxPrefix:    TmuxSocketDir,
	} {
		for i := 0; i < 50; i++ {
			dir, err := os.MkdirTemp(root, prefix)
			if err != nil {
				t.Fatal(err)
			}
			if got := Classify(filepath.Base(dir)); got != want {
				t.Fatalf("Classify(%q) = %v, want %v", filepath.Base(dir), got, want)
			}
		}
	}
}

func TestClassifyRejectsNamesTheHarnessDoesNotMake(t *testing.T) {
	for _, name := range []string{
		"",
		"af-",
		"af-1450838862",         // testguard.SocketTempDir: arbitrary content, not this shape
		"af-test-home-",         // no suffix
		"af-tmux-",              // no suffix
		"af-tmux-pkg-",          // no suffix, and must not read as IsolateTmux's "pkg-"
		"af-tmux-pkg-12a",       // not a MkdirTemp suffix
		"af-tmux-abc",           // ditto
		"af-test-home-1/2",      // a path, not a name
		"xaf-tmux-123",          // prefix must be the start
		"af-tmux-123 ",          // trailing junk
		"af-playtest-tmux-1234", // a different harness
		"af-tmux-١٢٣",           // Unicode digits are not what MkdirTemp writes
	} {
		if got := Classify(name); got != None {
			t.Errorf("Classify(%q) = %v, want None", name, got)
		}
	}
}
