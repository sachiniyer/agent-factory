package commands

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestParseHostDestinationForms pins #5970: a bracketed IPv6 SSH URL parses to the
// bare address plus its port, and every other documented form keeps its meaning.
func TestParseHostDestinationForms(t *testing.T) {
	cases := []struct {
		name, in string
		want     config.HostConfig
		wantErr  string
	}{
		{"ipv4", "10.0.0.7", config.HostConfig{Host: "10.0.0.7"}, ""},
		{"ipv4 user port", "ubuntu@10.0.0.7:2222", config.HostConfig{Host: "10.0.0.7", User: "ubuntu", Port: 2222}, ""},
		{"hostname", "box1.tailnet.ts.net", config.HostConfig{Host: "box1.tailnet.ts.net"}, ""},
		{"hostname user port", "ubuntu@box1.example:2200", config.HostConfig{Host: "box1.example", User: "ubuntu", Port: 2200}, ""},
		{"bare ipv6", "2001:db8::1", config.HostConfig{Host: "2001:db8::1"}, ""},
		{"bare ipv6 user", "ubuntu@2001:db8::1", config.HostConfig{Host: "2001:db8::1", User: "ubuntu"}, ""},
		{"url hostname", "ssh://ubuntu@box1.example:2222", config.HostConfig{Host: "box1.example", User: "ubuntu", Port: 2222}, ""},
		{"url trailing slash", "ssh://box1.example/", config.HostConfig{Host: "box1.example"}, ""},
		{"url ipv4", "ssh://ubuntu@10.0.0.7:2222", config.HostConfig{Host: "10.0.0.7", User: "ubuntu", Port: 2222}, ""},
		{"url bracketed ipv6 with port", "ssh://ubuntu@[2001:db8::1]:2222", config.HostConfig{Host: "2001:db8::1", User: "ubuntu", Port: 2222}, ""},
		{"url bracketed ipv6 no port", "ssh://ubuntu@[2001:db8::1]", config.HostConfig{Host: "2001:db8::1", User: "ubuntu"}, ""},
		{"url bracketed ipv6 no user", "ssh://[2001:db8::1]:2222/", config.HostConfig{Host: "2001:db8::1", Port: 2222}, ""},
		// The scheme-less forms keep their documented reading: more than one colon is
		// a whole IPv6 address, because without a URL there is nothing to bracket.
		{"scp-like bare ipv6 with user", "ubuntu@2001:db8::abcd:22", config.HostConfig{Host: "2001:db8::abcd:22", User: "ubuntu"}, ""},
		{"bracketed ipv6 with port", "ubuntu@[2001:db8::1]:2222", config.HostConfig{Host: "2001:db8::1", User: "ubuntu", Port: 2222}, ""},
		{"bracketed ipv6 no port", "[2001:db8::1]", config.HostConfig{Host: "2001:db8::1"}, ""},
		// #6067: an unbracketed IPv6 literal is not a URL authority. Refused with the
		// bracketed form named, never saved under a guessed host (net/url read
		// `...:abcd:22` as host 2001:db8::abcd on port 22).
		{"url unbracketed ipv6", "ssh://ubuntu@2001:db8::1", config.HostConfig{}, "ssh://ubuntu@[2001:db8::1]:22"},
		{"url unbracketed ipv6 looks like port", "ssh://ubuntu@2001:db8::abcd:22", config.HostConfig{}, "ssh://ubuntu@[2001:db8::abcd:22]:22"},
		{"url unbracketed ipv6 no user", "ssh://2001:db8::1/", config.HostConfig{}, "ssh://[2001:db8::1]:22"},
		{"url unbracketed ipv6 with port-like tail", "ssh://u@fe80::1:2222", config.HostConfig{}, "outside brackets"},
		{"url bad port", "ssh://box1.example:notaport", config.HostConfig{}, "ssh://"},
		{"url port out of range", "ssh://box1.example:70000", config.HostConfig{}, "1 to 65535"},
		{"url ipv6 bad port", "ssh://[2001:db8::1]:0", config.HostConfig{}, "1 to 65535"},
		{"url unclosed bracket", "ssh://ubuntu@[2001:db8::1:2222", config.HostConfig{}, "ssh://"},
		{"url path", "ssh://box1.example/etc", config.HostConfig{}, "no path"},
		{"url password", "ssh://u:pw@box1.example", config.HostConfig{}, "password"},
		{"bracket unclosed", "[2001:db8::1", config.HostConfig{}, "closing ]"},
		{"bracket junk after", "[2001:db8::1]x", config.HostConfig{}, "only a :port"},
		{"bracket bad port", "[2001:db8::1]:abc", config.HostConfig{}, "1 to 65535"},
		{"host bad port", "box:notaport", config.HostConfig{}, "1 to 65535"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHostDestination(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.NoError(t, config.ValidateHostEntry("box", got), "the stored entry must validate")
		})
	}
}

// TestSSHRemedyCommandForBracketedIPv6 closes the loop on the ssh argv: the
// stored bare address plus -p is what OpenSSH reads, parsed by `ssh -G` (which
// evaluates config and exits without connecting).
func TestSSHRemedyCommandForBracketedIPv6(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary")
	}
	hostsPosture = func() string { return "" }
	t.Cleanup(func() { hostsPosture = globalHostKeyPosture })
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	entry, err := parseHostDestination("ssh://ubuntu@[2001:db8::1]:2222")
	require.NoError(t, err)
	printed := sshRemedyCommand(entry)
	assert.NotContains(t, printed, "[", "brackets are URL syntax, not part of the ssh host")
	out, err := exec.Command("sh", "-c", strings.Replace(printed, "ssh ", "ssh -G ", 1)).CombinedOutput()
	require.NoError(t, err, "printed command must parse: %s\n%s", printed, out)
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			if _, seen := got[k]; !seen {
				got[k] = strings.TrimSpace(v)
			}
		}
	}
	assert.Equal(t, "2001:db8::1", got["hostname"], printed)
	assert.Equal(t, "ubuntu", got["user"], printed)
	assert.Equal(t, "2222", got["port"], printed)
}
