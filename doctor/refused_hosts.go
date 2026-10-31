package doctor

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// checkRefusedHosts reports the Host names the running daemon has refused
// recently (daemon/hostguard.go). The daemon answers only requests addressed to
// one of its own names, so the most likely reason for a refusal on a box that
// works is a reverse proxy — tailscale serve, Caddy — forwarding a public name
// that network.allowed_hosts does not list yet. Each row names the exact command
// that allows it, with the names already on disk kept in it, because `af config
// set` replaces the whole list.
//
// It is a warning, never a problem: doctor cannot tell a name the operator uses
// from one a DNS-rebinding page tried, so the row says both and leaves the choice
// to the operator. A name the config on disk already allows is skipped — the
// refusals came before that change.
func checkRefusedHosts(report *Report, refused []daemon.RefusedHost, cfg *config.Config) {
	var onDisk []string
	if cfg != nil {
		onDisk = cfg.AllowedHosts
	}
	allowed := make(map[string]bool, len(onDisk))
	for _, entry := range onDisk {
		if c, err := config.CanonicalAllowedHost(entry); err == nil {
			allowed[c] = true
		}
	}
	for _, r := range refused {
		if r.Name == "" || allowed[r.Name] {
			continue
		}
		list := append(append([]string{}, onDisk...), r.Name)
		report.Warn(sectionDaemon, "host names",
			fmt.Sprintf("the %s refused %d request(s) addressed to %q, which is not a name of this daemon (last at %s)",
				r.Listener, r.Count, r.Name, r.Last.Local().Format("2006-01-02 15:04:05")),
			fmt.Sprintf("if you reach af through that name on purpose (for example behind tailscale serve), allow it: "+
				"af config set %s %s — if you do not recognise it, leave it refused: that is the DNS-rebinding guard working",
				config.AllowedHostsKey, strings.Join(list, ",")),
			false)
	}
}
