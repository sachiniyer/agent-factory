package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runPlaytestPolicy(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	helper := filepath.Join(repoRoot(t), "scripts", "container", "playtest-lifetime.sh")
	argv := append([]string{"-c", "set -euo pipefail\nsource \"$1\"\nshift\n" + script, "test", helper}, args...)
	cmd := exec.CommandContext(ctx, "bash", argv...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("playtest policy exceeded test deadline: %s", out)
	}
	return string(out), err
}

func TestPlaytestLifetimeValidation(t *testing.T) {
	for _, value := range []string{"", "1", "21600", "2147483647", "0", "-1", "01", "1.5", "6h", " 1", "2147483648", "999999999999999999999"} {
		t.Run("value_"+value, func(t *testing.T) {
			out, err := runPlaytestPolicy(t, `playtest_lifetime "$1"`, value)
			valid := value == "" || value == "1" || value == "21600" || value == "2147483647"
			if (err == nil) != valid {
				t.Fatalf("value %q: err=%v output=%s", value, err, out)
			}
			if valid {
				want := value
				if want == "" {
					want = "21600"
				}
				if strings.TrimSpace(out) != want {
					t.Fatalf("got %q, want %q", out, want)
				}
			} else if !strings.Contains(out, "AF_PLAYTEST_MAX_LIFETIME") {
				t.Fatalf("missing actionable validation error: %s", out)
			}
		})
	}
}

func TestPlaytestExpiryPredicate(t *testing.T) {
	started := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	base := "/af-playtest-run|testbox|" + started.Format(time.RFC3339Nano) + "|detached|[]"
	legacy := "/af-playtest-run|testbox|" + started.Format(time.RFC3339Nano) + "||"
	cases := []struct {
		name, inspected string
		age             int64
		expired         bool
	}{
		{"before deadline", base, 21599, false},
		{"at deadline", base, 21600, true},
		{"after deadline", base, 21601, true},
		{"future start", base, -1, false},
		{"missing start", "/af-playtest-run|testbox||detached|[]", 21600, false},
		{"invalid start", "/af-playtest-run|testbox|not-a-date|detached|[]", 21600, false},
		{"never started", "/af-playtest-run|testbox|0001-01-01T00:00:00Z|detached|[]", 21600, false},
		{"unlabelled", "/af-playtest-run||2026-09-06T00:00:00Z|detached|[]", 21600, false},
		{"wrong label", "/af-playtest-run|other|2026-09-06T00:00:00Z|detached|[]", 21600, false},
		{"wrong name", "/af-testbox-run|testbox|2026-09-06T00:00:00Z|detached|[]", 21600, false},
		{"substring name", "/other-af-playtest-run|testbox|2026-09-06T00:00:00Z|detached|[]", 21600, false},
		{"long override", base + "\nAF_PLAYTEST_MAX_LIFETIME=43200", 21601, false},
		{"override boundary", base + "\nAF_PLAYTEST_MAX_LIFETIME=43200", 43200, true},
		{"short override", base + "\nAF_PLAYTEST_MAX_LIFETIME=1", 1, true},
		{"invalid override", base + "\nAF_PLAYTEST_MAX_LIFETIME=garbage", 43200, false},
		{"unrelated environment", base + "\nOTHER=AF_PLAYTEST_MAX_LIFETIME=1", 1, false},
		{"interactive label", strings.Replace(base, "|detached|", "|interactive|", 1), 43200, false},
		{"unknown mode", strings.Replace(base, "|detached|", "|unknown|", 1), 43200, false},
		{"legacy detached", legacy + `["bash","/src/scripts/container/playtest-entry.sh","hold"]`, 21600, true},
		{"legacy interactive", legacy + `["bash","/src/scripts/container/playtest-entry.sh"]`, 43200, false},
		{"legacy unknown command", legacy + `["sleep","infinity"]`, 43200, false},
		{"legacy missing command", legacy, 43200, false},
		{"interactive overrides command", strings.Replace(legacy, "||", "|interactive|", 1) + `["bash","/src/scripts/container/playtest-entry.sh","hold"]`, 43200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPlaytestPolicy(t, `
AF_PLAYTEST_MAX_LIFETIME=1
if playtest_is_expired "$1" "$2"; then echo expired; else echo retained; fi`, tc.inspected, strconv.FormatInt(started.Unix()+tc.age, 10))
			if err != nil {
				t.Fatalf("predicate: %v: %s", err, out)
			}
			if got := strings.HasSuffix(strings.TrimSpace(out), "expired"); got != tc.expired {
				t.Fatalf("expired=%v, want %v: %s", got, tc.expired, out)
			}
		})
	}
}

func TestPlaytestUnknownStartedAtFailsClosed(t *testing.T) {
	// GNU date accepts empty input as midnight and natural-language dates;
	// neither is a valid Docker StartedAt timestamp.
	for _, started := range []string{"", "yesterday", "2026-09-06"} {
		out, err := runPlaytestPolicy(t, `
if playtest_is_expired "$1" "$2"; then echo expired; else echo retained; fi`,
			"/af-playtest-run|testbox|"+started+"|detached|[]",
			strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))
		if err != nil || strings.TrimSpace(out) != "retained" {
			t.Errorf("StartedAt=%q: want retained, got %q, err=%v", started, out, err)
		}
	}
}

func TestPlaytestReaperUsesInspectedScopeAndAge(t *testing.T) {
	fixtures := t.TempDir()
	now := time.Now().UTC()
	old := now.Add(-7 * time.Hour).Format(time.RFC3339Nano)
	young := now.Add(-time.Minute).Format(time.RFC3339Nano)
	for id, inspected := range map[string]string{
		"old":                "/af-playtest-old|testbox|" + old + "|detached|[]",
		"young":              "/af-playtest-young|testbox|" + young + "|detached|[]",
		"unlabelled":         "/af-playtest-unlabelled||" + old + "|detached|[]",
		"other":              "/af-testbox-other|testbox|" + old + "|detached|[]",
		"extended":           "/af-playtest-long|testbox|" + old + "|detached|[]\nAF_PLAYTEST_MAX_LIFETIME=43200",
		"interactive":        "/af-playtest-interactive|testbox|" + old + "|interactive|[]",
		"legacy_interactive": "/af-playtest-legacy-interactive|testbox|" + old + `||["bash","/src/scripts/container/playtest-entry.sh"]`,
		"unknown":            "/af-playtest-unknown|testbox|" + old + `||["sleep","infinity"]`,
		"legacy":             "/af-playtest-legacy|testbox|" + old + `||["bash","/src/scripts/container/playtest-entry.sh","hold"]`,
	} {
		if err := os.WriteFile(filepath.Join(fixtures, id), []byte(inspected), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runPlaytestPolicy(t, `
fixtures="$1"
docker() {
    case "$1" in
        ps)
            [[ "$*" == 'ps -aq --filter label=af.harness=testbox --filter name=af-playtest-' ]] || return 90
            printf '%s\n' old young unlabelled other extended interactive legacy_interactive unknown legacy vanished
            ;;
        inspect)
            [[ "$3" == *'.State.StartedAt'* && "$3" == *'.Config.Labels'* && "$3" == *'af.playtest.mode'* && "$3" == *'json .Config.Cmd'* ]] || return 91
            cat "$fixtures/$4"
            ;;
        rm) printf '%s\n' "$*" >> "$fixtures/removals" ;;
        *) return 92 ;;
    esac
}
ENGINE=docker
reap_playtest_sandboxes
cat "$fixtures/removals"`, fixtures)
	if err != nil {
		t.Fatalf("reaper: %v: %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(fixtures, "removals"))
	if err != nil || string(data) != "rm -f old\nrm -f legacy\n" {
		t.Fatalf("removals=%q err=%v, want only expired detached sandboxes", data, err)
	}
}

func TestPlaytestDeadlineExpiresWithoutDriver(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("deadline runs inside the Linux container with GNU timeout")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("GNU timeout unavailable")
	}
	out, err := runPlaytestPolicy(t, `AF_PLAYTEST_MAX_LIFETIME=1 playtest_with_deadline sleep 3`)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 124 {
		t.Fatalf("deadline should expire (124), got %v: %s", err, out)
	}
	out, err = runPlaytestPolicy(t, `AF_PLAYTEST_MAX_LIFETIME=0 playtest_with_deadline echo must-not-run`)
	if err == nil || strings.Contains(out, "must-not-run") {
		t.Fatalf("invalid lifetime ran the command: %v: %s", err, out)
	}
}

func TestPlaytestHoldInstallsDeadlineBeforeSetup(t *testing.T) {
	// The fake timeout exits before the entrypoint can build or touch an AF home.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/bash\nprintf '%s\\n' \"$@\"\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Stop setup without side effects even if the deadline dispatch regresses.
	for _, name := range []string{"rm", "mkdir", "git", "go"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\nexit 99\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_PLAYTEST_MAX_LIFETIME", "17")
	entry := filepath.Join(repoRoot(t), "scripts", "container", "playtest-entry.sh")
	cmd := exec.Command("bash", entry, "hold")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("hold did not install deadline before setup: %v: %s", err, out)
	}
	want := "--signal=TERM\n--kill-after=5\n17\nbash\n" + entry + "\nhold-timed\n"
	if string(out) != want {
		t.Fatalf("deadline argv=%q, want %q", out, want)
	}
}
