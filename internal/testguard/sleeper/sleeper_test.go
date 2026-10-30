package sleeper

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The point of the package: a renamed copy is still a sleeper, whatever name it
// carries and whatever the host's own sleep does with a foreign name.
func TestNamedCopyRunsUnderAnyName(t *testing.T) {
	for _, name := range []string{"tmux", "af", "a) (b c"} {
		bin := Named(t, filepath.Join(t.TempDir(), name))
		cmd := exec.Command(bin, "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			t.Fatalf("%s exited at once: %v", name, err)
		case <-time.After(300 * time.Millisecond):
		}
		_ = cmd.Process.Kill()
		<-done
	}
}

func TestFractionalSecondsExit(t *testing.T) {
	if err := exec.Command(Path(t), "0.05").Run(); err != nil {
		t.Fatal(err)
	}
}
