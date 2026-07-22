package daemon

import (
	"strings"
	"testing"
)

// Golden-content tests for the daemon autostart units. `af daemon install`
// shells out to systemctl/launchctl, which CI cannot do, so the generation
// functions are pinned here instead.

func TestSystemdAutostartUnitContent(t *testing.T) {
	got := systemdAutostartUnit("/home/user/.local/bin/af", "/usr/bin:/bin", "/bin/zsh", "")
	want := `[Unit]
Description=Agent Factory daemon (task scheduler + session monitor)

[Service]
KillMode=process
StartLimitInterval=60
StartLimitBurst=5
ExecStart="/home/user/.local/bin/af" --daemon
Restart=on-failure
RestartSec=5
Environment=AGENT_FACTORY_SYSTEMD_UNIT=agent-factory-daemon.service
Environment=PATH=/usr/bin:/bin
Environment=SHELL=/bin/zsh

[Install]
WantedBy=default.target
`
	if got != want {
		t.Fatalf("systemd unit content mismatch.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestSystemdAutostartUnitBoundsRestartBurstCompatibly is the new-install half
// of #2168's Phase 1 backstop. RestartSec=5 outlives systemd's default
// 10-second start-limit window, so the default limiter can never catch a
// persistent crash. The legacy service-level spelling works before v229/v230
// and remains accepted by newer systemd releases.
func TestSystemdAutostartUnitBoundsRestartBurstCompatibly(t *testing.T) {
	got := systemdAutostartUnit("/home/user/.local/bin/af", "/usr/bin:/bin", "/bin/zsh", "")
	for _, want := range []string{
		"StartLimitInterval=60\n",
		"StartLimitBurst=5\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("systemd unit must contain exactly one %q:\n%s", strings.TrimSpace(want), got)
		}
	}
	if strings.Contains(got, "StartLimitIntervalSec=") {
		t.Fatalf("generated unit uses the v230-only interval spelling:\n%s", got)
	}
}

// TestSystemdAutostartUnitEscapesSpecials pins the systemd quoting rules: a
// binary path with spaces must stay one ExecStart argument; '%' must be
// doubled in Environment values so systemd does not expand it as a specifier;
// '$' must be left literal because Environment= (unlike ExecStart=) performs
// no variable expansion, so doubling it would corrupt the value (#893).
func TestSystemdAutostartUnitEscapesSpecials(t *testing.T) {
	got := systemdAutostartUnit("/opt/my tools/af", "/usr/bin:/home/u/100%path:$HOME/bin", "/bin/bash", "")
	if !strings.Contains(got, `ExecStart="/opt/my tools/af" --daemon`) {
		t.Errorf("path with spaces must be quoted in ExecStart, got:\n%s", got)
	}
	if !strings.Contains(got, "Environment=PATH=/usr/bin:/home/u/100%%path:$HOME/bin") {
		t.Errorf("%% must be doubled and $ left literal in Environment values, got:\n%s", got)
	}
}

// TestSystemdAutostartUnitQuotesSpacedEnvValues is the #893 regression guard:
// PATH/SHELL/AGENT_FACTORY_HOME values containing spaces must wrap the whole
// NAME=value in double quotes so systemd parses one assignment instead of
// splitting on the space and truncating the value.
func TestSystemdAutostartUnitQuotesSpacedEnvValues(t *testing.T) {
	got := systemdAutostartUnit("/home/user/.local/bin/af", "/usr/bin:/My Apps/bin", "/bin/zsh", "/srv/af home")
	for _, want := range []string{
		`Environment="PATH=/usr/bin:/My Apps/bin"`,
		"Environment=SHELL=/bin/zsh",
		`Environment="AGENT_FACTORY_HOME=/srv/af home"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The unquoted, truncating form must not appear for the spaced PATH.
	if strings.Contains(got, "Environment=PATH=/usr/bin:/My Apps/bin") &&
		!strings.Contains(got, `Environment="PATH=/usr/bin:/My Apps/bin"`) {
		t.Errorf("spaced PATH must be quoted, got:\n%s", got)
	}
}

// TestSystemdAutostartUnitEscapesQuotesAndBackslashes pins the in-quote
// C-escaping: a value carrying a double quote or backslash (alongside a space,
// which triggers quoting) must escape those characters so the wrapped value
// round-trips through systemd's unescaping parser.
func TestSystemdAutostartUnitEscapesQuotesAndBackslashes(t *testing.T) {
	got := systemdAutostartUnit("/home/user/.local/bin/af", `/usr/bin:/a "b"/x:/c\d/y`, "/bin/zsh", "")
	want := `Environment="PATH=/usr/bin:/a \"b\"/x:/c\\d/y"`
	if !strings.Contains(got, want) {
		t.Errorf("expected escaped+quoted PATH %q in:\n%s", want, got)
	}
}

func TestLaunchdAutostartPlistContent(t *testing.T) {
	got := launchdAutostartPlist("/Users/user/.local/bin/af", "/usr/bin:/bin", "/bin/zsh", "", "/Users/user/.agent-factory/daemon-launchd.log")
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.agent-factory.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Users/user/.local/bin/af</string>
        <string>--daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/bin:/bin</string>
        <key>SHELL</key>
        <string>/bin/zsh</string>
    </dict>
    <key>StandardOutPath</key>
    <string>/Users/user/.agent-factory/daemon-launchd.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/user/.agent-factory/daemon-launchd.log</string>
</dict>
</plist>
`
	if got != want {
		t.Fatalf("launchd plist content mismatch.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestLaunchdAutostartPlistEscapesXML guards against a path containing XML
// metacharacters breaking the plist.
func TestLaunchdAutostartPlistEscapesXML(t *testing.T) {
	got := launchdAutostartPlist("/opt/a&b/af", "/usr/bin", "/bin/sh", "", "/tmp/<log>.log")
	if !strings.Contains(got, "<string>/opt/a&amp;b/af</string>") {
		t.Errorf("& must be XML-escaped in the binary path, got:\n%s", got)
	}
	if !strings.Contains(got, "<string>/tmp/&lt;log&gt;.log</string>") {
		t.Errorf("< and > must be XML-escaped in the log path, got:\n%s", got)
	}
}

// TestSystemdAutostartUnitCapturesAgentFactoryHome pins the #782 phase-2 nit:
// when the installing shell has AGENT_FACTORY_HOME set, the unit must carry it
// into the daemon's environment, or the supervised daemon would serve the
// default home instead of the custom one.
func TestSystemdAutostartUnitCapturesAgentFactoryHome(t *testing.T) {
	got := systemdAutostartUnit("/home/user/.local/bin/af", "/usr/bin:/bin", "/bin/zsh", "/srv/af-home")
	want := `[Unit]
Description=Agent Factory daemon (task scheduler + session monitor)

[Service]
KillMode=process
StartLimitInterval=60
StartLimitBurst=5
ExecStart="/home/user/.local/bin/af" --daemon
Restart=on-failure
RestartSec=5
Environment=AGENT_FACTORY_SYSTEMD_UNIT=agent-factory-daemon.service
Environment=PATH=/usr/bin:/bin
Environment=SHELL=/bin/zsh
Environment=AGENT_FACTORY_HOME=/srv/af-home

[Install]
WantedBy=default.target
`
	if got != want {
		t.Fatalf("systemd unit content mismatch.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestLaunchdAutostartPlistCapturesAgentFactoryHome is the launchd variant of
// the AGENT_FACTORY_HOME capture, including XML escaping of the value.
func TestLaunchdAutostartPlistCapturesAgentFactoryHome(t *testing.T) {
	got := launchdAutostartPlist("/Users/user/.local/bin/af", "/usr/bin:/bin", "/bin/zsh", "/Users/user/af homes/<a&b>", "/Users/user/.agent-factory/daemon-launchd.log")
	wantEntry := `        <key>SHELL</key>
        <string>/bin/zsh</string>
        <key>AGENT_FACTORY_HOME</key>
        <string>/Users/user/af homes/&lt;a&amp;b&gt;</string>
    </dict>`
	if !strings.Contains(got, wantEntry) {
		t.Fatalf("plist must carry an escaped AGENT_FACTORY_HOME inside EnvironmentVariables, got:\n%s", got)
	}
}
