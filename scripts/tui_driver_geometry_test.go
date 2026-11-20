package main

// #7669: tui-driver.sh's geometry model must describe the LIVE tmux window
// whenever the driver session exists. A multi-phase scenario runs each
// phase in a fresh `docker exec` shell; before the fix, every fresh shell
// fell back to the 100x30 launch defaults even when the real `drive`
// session sat at 80x24, so _af_rail_title_avail sized a rail the TUI was
// not drawing and every _af_row_re helper (af_new_instance, af_select, …)
// false-timed-out on a healthy screen.
//
// The tests below are hermetic: a fake `tmux` binary on PATH answers the
// same `display-message -p '#{window_width}x#{window_height}'` read
// af_resize verifies with, and records resizes in a file so a LATER bash
// process — the analogue of a later `docker exec` — sees the new size.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fake tmux: logs every call to $FAKE_TMUX_LOG, keeps the "live" window
// size in $FAKE_TMUX_SIZE_FILE (seeded from $FAKE_TMUX_SIZE; missing file +
// empty seed = no server/session → display-message fails like the real
// thing), and answers capture-pane with the minimal screen af_boot's three
// gates require (title, rail marker, a border-free menu row with `n new`).
const tuiDriverFakeTmux = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_TMUX_LOG"
case " $* " in
  *" new-session "*|*" resize-window "*)
    cols=""; rows=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -x) cols="$2"; shift 2 ;;
        -y) rows="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ -n "$cols" ] && [ -n "$rows" ] \
      && printf '%sx%s\n' "$cols" "$rows" > "$FAKE_TMUX_SIZE_FILE"
    ;;
  *" display-message "*)
    if [ -f "$FAKE_TMUX_SIZE_FILE" ]; then
      cat "$FAKE_TMUX_SIZE_FILE"
    elif [ -n "${FAKE_TMUX_SIZE:-}" ]; then
      printf '%s\n' "$FAKE_TMUX_SIZE"
    else
      exit 1
    fi
    ;;
  *" capture-pane "*)
    printf ' Agent Factory\n Sessions (0)\n n new · ? help · q quit\n'
    ;;
  *" load-buffer "*)
    cat >/dev/null
    ;;
esac
exit 0
`

// driverProbeEnv builds the hermetic environment for a bash probe that
// sources the real tui-driver.sh: the fake tmux first on PATH, all driver
// state under t.TempDir(), and AF_DRIVER_BIN pointed at a real executable
// so af_boot's readiness check passes instantly.
func driverProbeEnv(t *testing.T, liveSize string, pins map[string]string) (env []string, tmuxLog string) {
	t.Helper()
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(tuiDriverFakeTmux), 0755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	tmuxLog = filepath.Join(tmp, "tmux.log")
	env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp,
		"DRIVER=" + filepath.Join(repoRoot(t), "scripts", "tui-driver.sh"),
		"FAKE_TMUX_LOG=" + tmuxLog,
		"FAKE_TMUX_SIZE_FILE=" + filepath.Join(tmp, "live-size"),
		"FAKE_TMUX_SIZE=" + liveSize,
		"AF_DRIVER_BIN=/bin/true",
		"AF_DRIVER_STATE_DIR=" + filepath.Join(tmp, "driver-state"),
		"AGENT_FACTORY_HOME=" + filepath.Join(tmp, "af-home"),
	}
	for k, v := range pins {
		env = append(env, k+"="+v)
	}
	return env, tmuxLog
}

func runDriverProbe(t *testing.T, env []string, script string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tui-driver.sh is bash; not run on Windows")
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The geometry model after sourcing must equal the live window for every
// dimension the caller left unset; a dimension set before sourcing is an
// explicit choice — a launch pin — and is never rewritten.
func TestTuiDriverGeometryModelFollowsLiveWindow(t *testing.T) {
	probe := `source "$DRIVER" &&
  printf 'model=%sx%s\n' "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS"`

	t.Run("syncs to the live window on source (#7669)", func(t *testing.T) {
		env, _ := driverProbeEnv(t, "80x24", nil)
		out, err := runDriverProbe(t, env, probe)
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		if want := "model=80x24"; strings.TrimSpace(out) != want {
			t.Fatalf("a fresh shell against a live 80x24 session must model 80x24, got %q (want %q)", out, want)
		}
	})

	t.Run("keeps the launch defaults when no session exists", func(t *testing.T) {
		env, _ := driverProbeEnv(t, "", nil)
		out, err := runDriverProbe(t, env, probe)
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		if want := "model=100x30"; strings.TrimSpace(out) != want {
			t.Fatalf("no live session must leave the 100x30 defaults, got %q (want %q)", out, want)
		}
	})

	t.Run("an explicit size is never rewritten — it chooses model and launch", func(t *testing.T) {
		env, _ := driverProbeEnv(t, "80x24", map[string]string{
			"AF_DRIVER_COLS": "120",
			"AF_DRIVER_ROWS": "40",
		})
		out, err := runDriverProbe(t, env, probe)
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		if want := "model=120x40"; strings.TrimSpace(out) != want {
			t.Fatalf("an exported 120x40 pin must survive the live-window sync, got %q (want %q)", out, want)
		}
	})

	t.Run("only the unset dimension syncs — a partial pin keeps its half", func(t *testing.T) {
		env, _ := driverProbeEnv(t, "80x24", map[string]string{
			"AF_DRIVER_COLS": "120",
		})
		out, err := runDriverProbe(t, env, probe)
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		if want := "model=120x24"; strings.TrimSpace(out) != want {
			t.Fatalf("pinned COLS=120 must hold while unset ROWS tracks live 24, got %q (want %q)", out, want)
		}
	})
}

// A size an earlier shell's af_resize left behind is the live size the next
// shell must model — the exact multi-`docker exec` shape from #7669. The
// fake tmux's size file stands in for the live server across processes.
func TestTuiDriverGeometrySeesResizeFromALaterShell(t *testing.T) {
	env, _ := driverProbeEnv(t, "80x24", nil)

	out, err := runDriverProbe(t, env, `source "$DRIVER" && af_resize 60 15`)
	if err != nil {
		t.Fatalf("af_resize failed: %v\n%s", err, out)
	}

	out, err = runDriverProbe(t, env,
		`source "$DRIVER" && printf '%sx%s' "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS"`)
	if err != nil {
		t.Fatalf("second-shell probe failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "60x15" {
		t.Fatalf("a later shell must model the size an earlier af_resize left (60x15), got %q", out)
	}
}

// An export-time pin chooses the LAUNCH size too: record-demo.sh's
// export → source → af_boot shape, proven by the stub tmux's call log.
func TestTuiDriverLaunchPinChoosesBootGeometry(t *testing.T) {
	env, tmuxLog := driverProbeEnv(t, "80x24", map[string]string{
		"AF_DRIVER_COLS": "120",
		"AF_DRIVER_ROWS": "40",
	})

	out, err := runDriverProbe(t, env, `source "$DRIVER" && af_boot &&
  printf 'post=%sx%s' "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS"`)
	if err != nil {
		logBytes, _ := os.ReadFile(tmuxLog)
		t.Fatalf("af_boot failed: %v\n%s\ntmux calls: %s", err, out, logBytes)
	}
	if strings.TrimSpace(out) != "post=120x40" {
		t.Fatalf("after boot at the pinned size the model must hold 120x40, got %q", out)
	}

	logBytes, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{
		"new-session -d -s drive -x 120 -y 40",
		"resize-window -t drive -x 120 -y 40",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("af_boot must launch at the exported pin — missing %q in tmux log:\n%s", want, log)
		}
	}
}
