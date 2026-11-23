package app

import (
	"context"
	"net"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui"
)

// A daemon that could not bind its web listener (the configured address is
// already in use) keeps working, so nothing else in the TUI hints that the
// http://localhost:8443 the docs point at is not served (#6386). The notice
// below stands on the bottom bar — unlike a transient notice it does not fade —
// until the listener binds, and uses the same sentence `af doctor` prints. The
// same health read also resolves the web UI's address for the web/vscode tab
// placeholders (#7811): they render the listener the daemon is ACTUALLY bound
// to, or a docs pointer when that is unknown — never the default guessed at.

// webListenerPollInterval paces the daemon health read. The listener state
// changes rarely (a bind retry succeeding), so this stays far off the snapshot
// loop's 750ms.
const webListenerPollInterval = 15 * time.Second

// webListenerNoticeMsg carries one health read back to the event loop: the
// standing notice for the bottom bar plus the freshest web UI hint for the tab
// placeholders. notice is "" and hint is zero when the listener is bound
// without trouble, disabled, still starting, remote, or unreadable.
type webListenerNoticeMsg struct {
	notice string
	hint   ui.WebUIHint
}

// fetchWebListenerPoll is the production reader of the daemon's listener
// status; newHome installs it as home.webListenerFetcher. It is a per-home field
// rather than a shared global for the reason snapshotFetcher is: the poll runs
// off the event loop, and a test harness that runs Init must be able to close
// the seam on its own home without racing another test's swap.
func fetchWebListenerPoll() webListenerNoticeMsg {
	var msg webListenerNoticeMsg
	if isRemoteTarget() {
		// A remote daemon's listener is the remote operator's concern, and its
		// bound address names a socket on THAT host — not one this machine's
		// browser can open — so the hint stays unknown for the pointer text too.
		return msg
	}
	_ = withDaemonHTTP(func(c *apiclient.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := c.Health(ctx)
		if err != nil {
			return err
		}
		msg.notice = resp.Listeners.WebListenerStandingNotice()
		msg.hint = webUIHintFrom(resp)
		return nil
	})
	return msg
}

// webUIHintFrom resolves a health read to the pane's web UI location. It only
// ever names an address the daemon reported itself bound to — TCPBoundAddr is
// the kernel's endpoint — so a listener that was refused, failed to bind, or is
// still retrying yields the unknown hint (a docs pointer) rather than a guessed
// one. An empty Phase means the responder predates listener reporting: its zero
// Listeners block would otherwise read as "off", which is a wrong claim, so it
// is treated as unknown too.
func webUIHintFrom(resp daemon.PingResponse) ui.WebUIHint {
	l := resp.Listeners
	switch {
	case resp.Phase == "":
		return ui.WebUIHint{}
	case !l.TCPConfigured:
		// network.listen_addr is "": the web server is deliberately off, so
		// there is no address to name — only the fact that it is off.
		return ui.WebUIHint{Disabled: true}
	case l.TCPBound && l.TCPBoundAddr != "":
		return ui.WebUIHint{URL: "http://" + browseableWebAddr(l.TCPBoundAddr)}
	default:
		return ui.WebUIHint{}
	}
}

// browseableWebAddr rewrites a wildcard bound host to the name a browser can
// open: 0.0.0.0/::/"" are bind addresses, not destinations, and a wildcard
// listener accepts loopback, so localhost reaches it. Any other bound host is
// shown verbatim — it is the address the daemon is actually serving.
func browseableWebAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		return net.JoinHostPort("localhost", port)
	}
	return addr
}

// currentWebUIHint is the provider installed on every pane: the freshest hint
// the health poll stored, read through an atomic so a placeholder composing off
// the event loop never races the poll's updates.
func (m *home) currentWebUIHint() ui.WebUIHint {
	if hint := m.webUIHint.Load(); hint != nil {
		return *hint
	}
	return ui.WebUIHint{}
}

// webListenerNoticeCmd reads the listener status after delay, off the event loop.
// fetch is captured on the event loop by the caller. A nil fetch is "no poll": the
// zero value of a home must never dial — or spawn — a daemon. The wait ends with
// ctx, and the read is skipped if ctx ended during it, so a torn-down TUI never
// reaches the daemon.
func webListenerNoticeCmd(ctx context.Context, fetch func() webListenerNoticeMsg, delay time.Duration) tea.Cmd {
	if fetch == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if ctx.Err() != nil {
			return nil
		}
		return fetch()
	}
}

// handleWebListenerNotice applies one read and re-arms the poll. It reports the
// state on the bar, and nothing more: no retained notice, no log spam. The
// notice is recorded on the model and the standing line is recomposed — never
// written straight into ErrBox's single standing slot, where a concurrent
// skipped-repo condition (#6986) would be erased, or erase it in turn. The hint
// replaces wholesale so a stale address can never outlive the read that moved
// it (a failed read stores the unknown hint — the placeholders point at the
// docs rather than keep naming an address that may be gone).
func (m *home) handleWebListenerNotice(msg webListenerNoticeMsg) (tea.Model, tea.Cmd) {
	m.webListenerNotice = msg.notice
	m.webUIHint.Store(&msg.hint)
	m.applyStandingNotice()
	return m, webListenerNoticeCmd(m.ctx, m.webListenerFetcher, webListenerPollInterval)
}
