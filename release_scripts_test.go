package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests exercise the release version-computation scripts used by the
// GitHub workflows (#1041). They are hermetic: tags are fed on stdin, no git
// or network involved.

// runVersionScript runs .github/scripts/<script> with args, feeding tags
// (one per line) on stdin. Returns trimmed stdout and any error.
func runVersionScript(t *testing.T, script string, args []string, tags []string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release scripts are POSIX sh; not run on Windows")
	}
	path := filepath.Join(".github", "scripts", script)
	cmd := exec.Command("sh", append([]string{path}, args...)...)
	cmd.Stdin = strings.NewReader(strings.Join(tags, "\n") + "\n")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestNextPreviewVersionScript(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{
			name: "first preview after a stable",
			tags: []string{"v1.0.136", "v1.0.137"},
			want: "1.0.138-preview-1",
		},
		{
			name: "increments z within a base",
			tags: []string{"v1.0.137", "v1.0.138-preview-1", "v1.0.138-preview-2"},
			want: "1.0.138-preview-3",
		},
		{
			name: "z compares numerically not lexically",
			tags: []string{"v1.0.137", "v1.0.138-preview-9", "v1.0.138-preview-10"},
			want: "1.0.138-preview-11",
		},
		{
			name: "stable compares numerically not lexically",
			tags: []string{"v1.0.9", "v1.0.10"},
			want: "1.0.11-preview-1",
		},
		{
			name: "z resets when a new stable changes the base",
			tags: []string{"v1.0.137", "v1.0.138-preview-5", "v1.1.0"},
			want: "1.1.1-preview-1",
		},
		{
			name: "promoted preview base moves previews to the next patch",
			tags: []string{"v1.0.137", "v1.0.138-preview-3", "v1.0.138"},
			want: "1.0.139-preview-1",
		},
		{
			name: "ignores tags matching neither channel",
			tags: []string{"foo", "v1.2", "v1.0.138-rc-1", "v1.0.137", "v1.0.138-preview-x"},
			want: "1.0.138-preview-1",
		},
		{
			name: "no tags at all",
			tags: []string{},
			want: "0.0.1-preview-1",
		},
	}
	for _, c := range cases {
		got, err := runVersionScript(t, "next-preview-version.sh", nil, c.tags)
		if err != nil {
			t.Errorf("%s: script failed: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: next preview = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidateStableVersionScript(t *testing.T) {
	cases := []struct {
		name    string
		version string
		tags    []string
		wantOK  bool
	}{
		{
			name:    "valid bump over latest stable",
			version: "1.1.0",
			tags:    []string{"v1.0.136", "v1.0.137"},
			wantOK:  true,
		},
		{
			name:    "promoting the current preview base is allowed",
			version: "1.0.138",
			tags:    []string{"v1.0.137", "v1.0.138-preview-9"},
			wantOK:  true,
		},
		{
			name:    "equal to latest stable is rejected",
			version: "1.0.137",
			tags:    []string{"v1.0.136", "v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "lower than latest stable is rejected",
			version: "1.0.100",
			tags:    []string{"v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "leading v is rejected",
			version: "v1.1.0",
			tags:    []string{"v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "two-part version is rejected",
			version: "1.1",
			tags:    []string{"v1.0.137"},
			wantOK:  false,
		},
		{
			name:    "already-tagged version is rejected",
			version: "1.1.0",
			tags:    []string{"v1.0.137", "v1.1.0"},
			wantOK:  false,
		},
		{
			name:    "numeric comparison against latest stable",
			version: "1.0.11",
			tags:    []string{"v1.0.9", "v1.0.10"},
			wantOK:  true,
		},
	}
	for _, c := range cases {
		_, err := runVersionScript(t, "validate-stable-version.sh", []string{c.version}, c.tags)
		if ok := err == nil; ok != c.wantOK {
			t.Errorf("%s: validate %q ok=%v, want ok=%v (err: %v)", c.name, c.version, ok, c.wantOK, err)
		}
	}
}

func TestInstallScriptRestartsDaemonWithInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh; not run on Windows")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fakebin")
	installDir := filepath.Join(dir, "install")
	callsFile := filepath.Join(dir, "af-calls")

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
printf fake-tarball > "$out"
`)
	writeExecutable(t, filepath.Join(fakeBin, "tar"), `#!/bin/sh
case "$1" in
	tzf)
		exit 0
		;;
	xzf)
		outdir=
		shift 2
		while [ "$#" -gt 0 ]; do
			if [ "$1" = "-C" ]; then
				outdir="$2"
				break
			fi
			shift
		done
		[ -n "$outdir" ] || exit 2
		cat > "$outdir/agent-factory" <<'AF'
#!/bin/sh
printf '%s\n' "$*" >> "$AF_FAKE_AF_CALLS"
if [ "$1" = "version" ]; then
	echo "agent-factory version fake"
fi
exit 0
AF
		chmod +x "$outdir/agent-factory"
		;;
	*)
		exit 2
		;;
esac
`)

	cmd := exec.Command("sh", "install.sh", "--version", "v9.9.9")
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AF_INSTALL_DIR="+installDir,
		"AF_FAKE_AF_CALLS="+callsFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}

	assertScriptRestartCall(t, callsFile)
}

func TestDevInstallScriptRestartsDaemonWithInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dev-install.sh is POSIX sh; not run on Windows")
	}
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fakebin")
	installDir := filepath.Join(dir, "install")
	callsFile := filepath.Join(dir, "af-calls")

	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		out="$2"
		break
	fi
	shift
done
[ -n "$out" ] || exit 2
cat > "$out" <<'AF'
#!/bin/sh
printf '%s\n' "$*" >> "$AF_FAKE_AF_CALLS"
if [ "$1" = "version" ]; then
	echo "agent-factory version fake"
fi
exit 0
AF
chmod +x "$out"
`)

	scriptPath, err := filepath.Abs("dev-install.sh")
	if err != nil {
		t.Fatalf("resolve dev-install.sh: %v", err)
	}
	cmd := exec.Command("sh", scriptPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BIN_DIR="+installDir,
		"AF_FAKE_AF_CALLS="+callsFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dev-install.sh failed: %v\n%s", err, out)
	}

	assertScriptRestartCall(t, callsFile)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir fake tool dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write fake tool %s: %v", path, err)
	}
}

func assertScriptRestartCall(t *testing.T, callsFile string) {
	t.Helper()
	raw, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatalf("read fake af calls: %v", err)
	}
	calls := string(raw)
	if !strings.Contains(calls, "version\n") {
		t.Fatalf("installed af version command was not called; calls:\n%s", calls)
	}
	if !strings.Contains(calls, "daemon restart --quiet\n") {
		t.Fatalf("installed af daemon restart --quiet was not called; calls:\n%s", calls)
	}
}
