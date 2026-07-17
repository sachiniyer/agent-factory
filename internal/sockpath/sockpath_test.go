package sockpath

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The socket names the daemon binds. Mirrored here rather than imported
// (daemon imports this package, not the other way round); the arithmetic below
// is about these exact lengths, so a rename that changes them should fail here
// and be re-reasoned rather than silently shift the margins.
const (
	daemonSock     = "daemon.sock"
	daemonHTTPSock = "daemon-http.sock" // the longest, so the first to overrun
)

// TestPlatformCeilingMatchesKernel pins the two numbers everything else rests
// on. They come from x/sys's RawSockaddrUnix, generated from each platform's
// headers, so this is really asserting that we read the ABI rather than
// guessing at it.
func TestPlatformCeilingMatchesKernel(t *testing.T) {
	want := map[string]int{
		"linux":  107, // sun_path 108
		"darwin": 103, // sun_path 104
	}[runtime.GOOS]
	if want == 0 {
		t.Skipf("no expected sun_path size recorded for %s", runtime.GOOS)
	}
	if Max != want {
		t.Errorf("Max on %s = %d, want %d — the socket ceiling must match the platform's sun_path",
			runtime.GOOS, Max, want)
	}
}

// TestPortableIsDarwinsCeiling documents WHY Portable is 103: darwin's
// sun_path is the smallest among the platforms we ship, so it is the binding
// constraint for anything that must work on both.
func TestPortableIsDarwinsCeiling(t *testing.T) {
	if Portable != 103 {
		t.Errorf("Portable = %d, want 103 (darwin's 104-byte sun_path is the floor across the platforms we ship)", Portable)
	}
	if Portable > Max {
		t.Errorf("Portable (%d) exceeds this platform's Max (%d) — the portable floor can never be looser than a real kernel", Portable, Max)
	}
}

// TestLinuxCeilingIsNotTightenedToPortable guards a specific regression this
// fix could have caused. The old hardcoded constant was the portable 103, and
// applying THAT to the daemon's sockets would have rejected Linux paths of
// 104–107 bytes that the Linux kernel binds happily — breaking installs that
// work today, in the name of fixing an error message. The product bound must be
// the kernel's, not the portable floor.
func TestLinuxCeilingIsNotTightenedToPortable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-specific: the loosening only applies where sun_path is 108")
	}
	path := "/" + strings.Repeat("a", 106) // 107 bytes: legal on Linux, too long on darwin
	if err := Check("test socket", path); err != nil {
		t.Errorf("Check(%d-byte path) = %v, want nil — Linux binds up to %d bytes and af must not reject what the kernel accepts",
			len(path), err, Max)
	}
}

// macHome renders an AF home the way it resolves on a Mac.
func macHome(user, sub string) string { return filepath.Join("/Users", user, sub) }

// TestRealisticMacHomeArithmetic is the reachability question #1940 asked,
// worked rather than guessed: can a real Mac user land past sun_path and get a
// daemon that will not start?
//
// It asserts against darwin's 103-byte ceiling (== Portable) on every runner,
// so the mac reality is checked even on Linux CI.
//
// The answer this locks: NOT on a default install — that needs a 65-character
// username — but YES for someone who points AGENT_FACTORY_HOME at iCloud Drive,
// which clears the ceiling by two bytes and overruns one subdirectory deeper.
// That is the case the guard exists for.
func TestRealisticMacHomeArithmetic(t *testing.T) {
	const darwinMax = Portable // 103; darwin's real ceiling

	for _, tc := range []struct {
		name     string
		home     string
		wantFits bool
	}{
		{
			name:     "default home, our real Mac user",
			home:     macHome("pradeepiyer", ".agent-factory"),
			wantFits: true,
		},
		{
			name:     "default home, firstname.lastname",
			home:     macHome("firstname.lastname", ".agent-factory"),
			wantFits: true,
		},
		{
			// macOS's own config location; os.UserConfigDir() returns it.
			name:     "~/Library/Application Support, firstname.lastname",
			home:     macHome("firstname.lastname", "Library/Application Support/agent-factory"),
			wantFits: true,
		},
		{
			// A plausible "sync my config across machines" move. 101 bytes:
			// it fits, by two.
			name:     "iCloud Drive, firstname.lastname",
			home:     macHome("firstname.lastname", "Library/Mobile Documents/com~apple~CloudDocs/agent-factory"),
			wantFits: true,
		},
		{
			// One directory deeper and the daemon cannot bind. This is the
			// user #1940 is for.
			name:     "iCloud Drive + one subdir, firstname.lastname",
			home:     macHome("firstname.lastname", "Library/Mobile Documents/com~apple~CloudDocs/agent-factory/home"),
			wantFits: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tc.home, daemonHTTPSock)
			fits := len(path) <= darwinMax
			if fits != tc.wantFits {
				t.Errorf("%s is %d bytes; fits under darwin's %d-byte ceiling = %v, want %v",
					path, len(path), darwinMax, fits, tc.wantFits)
			}
		})
	}
}

// TestDefaultInstallNeedsAnAbsurdUsernameToOverrun is the other half of the
// verdict, and it is what stops this being filed as "af doesn't work on my
// Mac". A default install is 39 bytes plus the username, so it takes a
// 65-character username to breach darwin's ceiling. The guard must therefore
// never fire for an ordinary user — a spurious guard on the daemon's startup
// path would break af for everyone, which is worse than the bug it prevents.
func TestDefaultInstallNeedsAnAbsurdUsernameToOverrun(t *testing.T) {
	const darwinMax = Portable

	overhead := len(filepath.Join("/Users", "", ".agent-factory", daemonHTTPSock)) - len("/")
	firstBreaking := darwinMax - overhead + 1
	if firstBreaking < 60 {
		t.Errorf("a default install overruns at a %d-character username; that is short enough to be a real "+
			"default-install breakage and #1940's severity needs re-reasoning", firstBreaking)
	}

	// A 40-character username — already absurd — must still fit.
	path := filepath.Join(macHome(strings.Repeat("u", 40), ".agent-factory"), daemonHTTPSock)
	if len(path) > darwinMax {
		t.Errorf("a 40-char username overruns (%d bytes) — the default install is tighter than the verdict claims", len(path))
	}
}

// TestCheckNamesTheCauseAndTheKnob is the point of the whole guard. The kernel
// already refuses to bind an over-long path; what it will not do is say why.
// "bind: invalid argument" names neither the path, its length, the limit, nor
// the one setting that fixes it — so a user has no route from the error to the
// cause. If this message ever loses those four things, the guard has stopped
// being worth having.
func TestCheckNamesTheCauseAndTheKnob(t *testing.T) {
	path := "/Users/someone/" + strings.Repeat("deep/", 30) + daemonSock
	err := Check("daemon control socket", path)
	if err == nil {
		t.Fatalf("Check(%d-byte path) = nil, want an error", len(path))
	}
	for _, want := range []string{
		"daemon control socket", // which socket
		path,                    // the path itself
		"AGENT_FACTORY_HOME",    // the knob that fixes it
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check error %q does not mention %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "unix socket") {
		t.Errorf("Check error %q does not explain that the limit is a unix socket limit", err)
	}
}

// TestCheckAllowsExactlyTheCeiling pins the off-by-one. sun_path holds Max
// bytes plus a NUL, so a path of exactly Max must bind. Rejecting it would fail
// installs the kernel accepts — the same class of harm as the bug.
func TestCheckAllowsExactlyTheCeiling(t *testing.T) {
	exact := "/" + strings.Repeat("a", Max-1)
	if len(exact) != Max {
		t.Fatalf("test built a %d-byte path, want %d", len(exact), Max)
	}
	if err := Check("test socket", exact); err != nil {
		t.Errorf("Check(exactly Max=%d bytes) = %v, want nil", Max, err)
	}
	if err := Check("test socket", exact+"b"); err == nil {
		t.Errorf("Check(Max+1=%d bytes) = nil, want an error", Max+1)
	}
}
