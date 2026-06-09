package daemon

import (
	"strings"
	"testing"
)

// Golden-content tests for the daemon autostart units. `af daemon install`
// shells out to systemctl/launchctl, which CI cannot do, so the generation
// functions are pinned here instead.

func TestSystemdAutostartUnitContent(t *testing.T) {
	got := systemdAutostartUnit("/home/user/.local/bin/af", "/usr/bin:/bin", "/bin/zsh")
	want := `[Unit]
Description=Agent Factory daemon (task scheduler + autoyes)

[Service]
ExecStart="/home/user/.local/bin/af" --daemon
Restart=on-failure
RestartSec=5
Environment=PATH=/usr/bin:/bin
Environment=SHELL=/bin/zsh

[Install]
WantedBy=default.target
`
	if got != want {
		t.Fatalf("systemd unit content mismatch.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestSystemdAutostartUnitEscapesSpecials pins the systemd quoting rules: a
// binary path with spaces must stay one ExecStart argument, and % / $ must
// be doubled so systemd does not expand them as specifiers/variables.
func TestSystemdAutostartUnitEscapesSpecials(t *testing.T) {
	got := systemdAutostartUnit("/opt/my tools/af", "/usr/bin:/home/u/100%path:$HOME/bin", "/bin/bash")
	if !strings.Contains(got, `ExecStart="/opt/my tools/af" --daemon`) {
		t.Errorf("path with spaces must be quoted in ExecStart, got:\n%s", got)
	}
	if !strings.Contains(got, "Environment=PATH=/usr/bin:/home/u/100%%path:$$HOME/bin") {
		t.Errorf("%% and $ must be escaped in Environment values, got:\n%s", got)
	}
}

func TestLaunchdAutostartPlistContent(t *testing.T) {
	got := launchdAutostartPlist("/Users/user/.local/bin/af", "/usr/bin:/bin", "/bin/zsh", "/Users/user/.agent-factory/daemon-launchd.log")
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
	got := launchdAutostartPlist("/opt/a&b/af", "/usr/bin", "/bin/sh", "/tmp/<log>.log")
	if !strings.Contains(got, "<string>/opt/a&amp;b/af</string>") {
		t.Errorf("& must be XML-escaped in the binary path, got:\n%s", got)
	}
	if !strings.Contains(got, "<string>/tmp/&lt;log&gt;.log</string>") {
		t.Errorf("< and > must be XML-escaped in the log path, got:\n%s", got)
	}
}
