package app

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
)

// A daemon that could not bind its web listener (the configured address is
// already in use) keeps working, so nothing else in the TUI hints that the
// http://localhost:8443 the docs point at is not served (#6386). The notice
// below stands on the bottom bar — unlike a transient notice it does not fade —
// until the listener binds, and uses the same sentence `af doctor` prints.

// webListenerPollInterval paces the daemon health read. The listener state
// changes rarely (a bind retry succeeding), so this stays far off the snapshot
// loop's 750ms.
const webListenerPollInterval = 15 * time.Second

// webListenerNoticeMsg carries one health read back to the event loop. notice
// is "" when the listener is bound, disabled, still starting, or unreadable.
type webListenerNoticeMsg struct{ notice string }

// fetchWebListenerNotice is the production reader of the daemon's listener
// status; newHome installs it as home.webListenerFetcher. It is a per-home field
// rather than a shared global for the reason snapshotFetcher is: the poll runs
// off the event loop, and a test harness that runs Init must be able to close
// the seam on its own home without racing another test's swap.
func fetchWebListenerNotice() string {
	if isRemoteTarget() {
		// A remote daemon's listener is the remote operator's concern.
		return ""
	}
	var notice string
	_ = withDaemonHTTP(func(c *apiclient.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := c.Health(ctx)
		if err != nil {
			return err
		}
		notice = resp.Listeners.WebListenerNotice()
		return nil
	})
	return notice
}

// webListenerNoticeCmd reads the listener status after delay, off the event loop.
// fetch is captured on the event loop by the caller. A nil fetch is "no poll": the
// zero value of a home must never dial — or spawn — a daemon. The wait ends with
// ctx, and the read is skipped if ctx ended during it, so a torn-down TUI never
// reaches the daemon.
func webListenerNoticeCmd(ctx context.Context, fetch func() string, delay time.Duration) tea.Cmd {
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
		return webListenerNoticeMsg{notice: fetch()}
	}
}

// handleWebListenerNotice applies one read and re-arms the poll. It reports the
// state on the bar, and nothing more: no retained notice, no log spam.
func (m *home) handleWebListenerNotice(msg webListenerNoticeMsg) (tea.Model, tea.Cmd) {
	m.errBox.SetStanding(msg.notice)
	return m, webListenerNoticeCmd(m.ctx, m.webListenerFetcher, webListenerPollInterval)
}
