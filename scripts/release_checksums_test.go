package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var checksumReleaseAssets = []string{
	"agent-factory-darwin-amd64.tar.gz",
	"agent-factory-darwin-arm64.tar.gz",
	"agent-factory-linux-amd64.tar.gz",
	"agent-factory-linux-arm64.tar.gz",
}

func TestReleaseWorkflowsPublishChecksumManifest(t *testing.T) {
	for _, workflow := range []string{"stable-release.yml", "auto-release.yml"} {
		workflow := workflow
		t.Run(workflow, func(t *testing.T) {
			contents, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", workflow))
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			text := string(contents)
			if !strings.Contains(text, ".github/scripts/write-sha256sums.sh dist") {
				t.Fatal("release workflow does not generate the shared checksum manifest")
			}
			if !strings.Contains(text, "dist/sha256sums.txt") {
				t.Fatal("release workflow does not upload sha256sums.txt")
			}
		})
	}
}

func TestWriteSHA256SumsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("write-sha256sums.sh is POSIX sh; not run on Windows")
	}
	dist := t.TempDir()
	var expected strings.Builder
	for _, asset := range checksumReleaseAssets {
		contents := []byte("archive-" + asset)
		writeFile(t, filepath.Join(dist, asset), string(contents))
		_, _ = fmt.Fprintf(&expected, "%x  %s\n", sha256.Sum256(contents), asset)
	}

	cmd := exec.Command("sh", filepath.Join(repoRoot(t), ".github", "scripts", "write-sha256sums.sh"), dist)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write-sha256sums.sh failed: %v\n%s", err, output)
	}
	manifest, err := os.ReadFile(filepath.Join(dist, "sha256sums.txt"))
	if err != nil {
		t.Fatalf("read checksum manifest: %v", err)
	}
	if string(manifest) != expected.String() {
		t.Fatalf("manifest = %q, want %q", manifest, expected.String())
	}
}

func TestWriteSHA256SumsScriptRejectsMissingArtifact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("write-sha256sums.sh is POSIX sh; not run on Windows")
	}
	dist := t.TempDir()
	for _, asset := range checksumReleaseAssets[:len(checksumReleaseAssets)-1] {
		writeFile(t, filepath.Join(dist, asset), "archive")
	}

	cmd := exec.Command("sh", filepath.Join(repoRoot(t), ".github", "scripts", "write-sha256sums.sh"), dist)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "release artifact is missing") {
		t.Fatalf("write-sha256sums.sh error = %v, output = %q; want missing-artifact failure", err, output)
	}
}

func TestInstallScriptRejectsChecksumMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh; not run on Windows")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fakebin")
	writeExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
	-s) echo Linux ;;
	-m) echo x86_64 ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		out="$2"
		break
	fi
	shift
done
[ -n "$out" ] || exit 2
case "$out" in
	*/sha256sums.txt)
		printf '%s\n' '0000000000000000000000000000000000000000000000000000000000000000  agent-factory-linux-amd64.tar.gz' > "$out"
		;;
	*)
		printf untrusted-archive > "$out"
		;;
esac
`)

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, "install.sh"), "--version", "v9.9.9")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AF_INSTALL_DIR="+filepath.Join(dir, "install"),
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "checksum mismatch") {
		t.Fatalf("install.sh error = %v, output = %q; want checksum mismatch", err, output)
	}
}

func runInstallWithoutChecksumManifest(t *testing.T, version string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fakebin")
	writeExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "$1" in
	-s) echo Linux ;;
	-m) echo x86_64 ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		out="$2"
		break
	fi
	shift
done
[ -n "$out" ] || exit 2
case "$out" in
	*/sha256sums.txt) exit 22 ;;
	*) printf legacy-archive > "$out" ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "sha256sum"), `#!/bin/sh
printf '%s  %s\n' '6db75e6ff648c3b8d75172a4720b9eb8b8477bd52cd753a85b2495b67ffc633b' "$1"
`)

	root := repoRoot(t)
	args := []string{filepath.Join(root, "install.sh")}
	if version != "" {
		args = append(args, "--version", version)
	}
	cmd := exec.Command("sh", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AF_INSTALL_DIR="+filepath.Join(dir, "install"),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestInstallScriptUsesPinnedChecksumForPreManifestLatest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh; not run on Windows")
	}
	output, err := runInstallWithoutChecksumManifest(t, "")
	if err == nil || !strings.Contains(output, "not a valid gzip/tar archive") {
		t.Fatalf("install.sh error = %v, output = %q; want checksum verification to reach archive validation", err, output)
	}
	if strings.Contains(output, "checksum manifest download failed") {
		t.Fatalf("default install rejected the pinned pre-manifest release: %q", output)
	}
}

func TestInstallScriptRejectsMissingManifestForUnpinnedRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh; not run on Windows")
	}
	output, err := runInstallWithoutChecksumManifest(t, "v9.9.9")
	if err == nil || !strings.Contains(output, "checksum manifest download failed") {
		t.Fatalf("install.sh error = %v, output = %q; want missing-manifest rejection", err, output)
	}
}
