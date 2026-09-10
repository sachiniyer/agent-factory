//go:build linux

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookProgressSuccessorReceiptWaitIsBounded(t *testing.T) {
	p := &hookProgress{Directory: t.TempDir()}
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	previousTimeout := hookStopTimeout
	hookStopTimeout = 2 * time.Second
	t.Cleanup(func() { hookStopTimeout = previousTimeout })

	started := time.Now()
	cmd := exec.Command("sh", p.command(0, "true")...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	select {
	case err := <-done:
		finished = true
		if exitCode(err) != 125 {
			t.Fatalf("successor exit = %v, want launch failure 125", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("successor spun forever after the winning shell left no exit receipt")
	}
	if elapsed := time.Since(started); elapsed < hookStopTimeout || elapsed > hookStopTimeout+2*time.Second {
		t.Fatalf("successor receipt wait took %s, want bound near %s", elapsed, hookStopTimeout)
	}
}

func TestHookProgressSuccessorUsesStrictPOSIXSleep(t *testing.T) {
	p := &hookProgress{Directory: t.TempDir()}
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	previousTimeout := hookStopTimeout
	hookStopTimeout = 2 * time.Second
	t.Cleanup(func() { hookStopTimeout = previousTimeout })

	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	calls, rejected := filepath.Join(dir, "calls"), filepath.Join(dir, "rejected")
	shim := `#!/bin/sh
case "${1:-}" in
  ''|*[!0-9]*) printf '%s\n' "${1:-}" >> ` + shellQuoteForShim(rejected) + `; exit 64 ;;
esac
printf '%s\n' "$1" >> ` + shellQuoteForShim(calls) + `
exec ` + shellQuoteForShim(realSleep) + ` "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	cmd := exec.Command("sh", p.command(0, "true")...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	time.Sleep(200 * time.Millisecond)
	if raw, err := os.ReadFile(rejected); err == nil && len(raw) > 0 {
		t.Fatalf("strict POSIX sleep rejected fractional polling and the wrapper hot-spun: %q", raw)
	}
	select {
	case err := <-done:
		finished = true
		if exitCode(err) != 125 {
			t.Fatalf("strict-POSIX successor exit = %v, want 125", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("strict-POSIX successor exceeded its receipt wait bound")
	}
	if elapsed := time.Since(started); elapsed < hookStopTimeout || elapsed > hookStopTimeout+2*time.Second {
		t.Fatalf("strict-POSIX receipt wait took %s, want bound near %s", elapsed, hookStopTimeout)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(raw))); got != 2 {
		t.Fatalf("strict POSIX sleep calls = %d, want two one-second polls", got)
	}
}

func TestHookProgressExitReceiptPublishesAtomically(t *testing.T) {
	p := &hookProgress{Directory: t.TempDir()}
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	shim := `#!/bin/sh
: > ` + shellQuoteForShim(entered) + `
while [ ! -f ` + shellQuoteForShim(release) + ` ]; do sleep 1; done
exec ` + shellQuoteForShim(realMV) + ` "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "mv"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}

	winner := exec.Command("sh", p.command(0, "exit 23")...)
	winner.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := winner.Start(); err != nil {
		t.Fatal(err)
	}
	winnerDone := make(chan error, 1)
	go func() { winnerDone <- winner.Wait() }()
	winnerFinished := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		if !winnerFinished {
			_ = winner.Process.Kill()
			<-winnerDone
		}
	})
	deadline := time.NewTimer(5 * time.Second)
	poll := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer poll.Stop()
	waiting := true
	for waiting {
		select {
		case err := <-winnerDone:
			winnerFinished = true
			t.Fatalf("exit receipt was written without entering atomic rename publication: %v", err)
		case <-poll.C:
			if _, err := os.Stat(entered); err == nil {
				waiting = false
			}
		case <-deadline.C:
			t.Fatal("exit receipt writer did not reach atomic publication")
		}
	}
	if _, err := os.Stat(filepath.Join(p.receipt(0), "exit")); !os.IsNotExist(err) {
		t.Fatalf("final exit receipt was visible before atomic rename: %v", err)
	}
	temporary, err := filepath.Glob(filepath.Join(p.receipt(0), ".exit-*"))
	if err != nil || len(temporary) != 1 {
		t.Fatalf("temporary exit receipts = %v, err=%v", temporary, err)
	}
	if raw, err := os.ReadFile(temporary[0]); err != nil || string(raw) != "23\n" {
		t.Fatalf("temporary exit receipt = %q, err=%v", raw, err)
	}

	successor := exec.Command("sh", p.command(0, "true")...)
	if err := successor.Start(); err != nil {
		t.Fatal(err)
	}
	successorDone := make(chan error, 1)
	go func() { successorDone <- successor.Wait() }()
	select {
	case err := <-successorDone:
		t.Fatalf("successor read a receipt before atomic publication: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-winnerDone; exitCode(err) != 23 {
		t.Fatalf("winning wrapper exit = %v, want 23", err)
	}
	winnerFinished = true
	if err := <-successorDone; exitCode(err) != 23 {
		t.Fatalf("successor wrapper exit = %v, want recorded status 23", err)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}
