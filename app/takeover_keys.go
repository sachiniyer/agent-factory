package app

import (
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The key routing of a bare-tmux takeover (the config assistant, the account
// login pane): ONE owner decides what every key does in the takeover's client, so
// the configured detach key cannot be lost to a second rule (#6064, #6074, #6075).
//
// A takeover is a real tmux client, so af's byte interception cannot see its keys.
// tmux keys its bindings by session, not by client, so the routing is made
// per-client by giving every takeover client ITS OWN session: a grouped session
// (`new-session -t <login session>`) that shares the windows but carries its own
// options. Everything below is set on that private session:
//
//   - key-table: a table named for the private session holds the detach binding.
//     No other client — a second takeover on the same login session included —
//     is attached to the private session, so none of them sees this binding, and
//     none of their bindings can be removed by this client leaving (#6075).
//   - the private session starts with the shared session's options (mouse
//     included), so scrolling and selection work in the takeover pane whatever the
//     server's global mouse setting is (#6073).
//   - the table starts as a copy of root, so the takeover pane keeps every binding
//     the session's normal pane has (mouse wheel and selection included) and gains
//     only the detach route (#6073).
//   - prefix: tmux consults the prefix before the table, so a detach key equal to
//     the prefix would never reach its binding (#6074). The takeover therefore
//     routes the prefix away from the detach key: the prefix is C-b, unless the
//     configured key IS C-b, in which case the prefix is disabled and the detach
//     key wins. prefix2 is always disabled. There is no collision left to refuse.
//   - teardown is explicit, never tmux's destroy-unattached: the reaper that
//     option arms killed the whole tmux server intermittently on tmux 3.3a when it
//     destroyed a grouped session whose source was being killed (#6193). The
//     client's exit runs the cleanup below, and the daemon kills a source
//     session's group mates with it (session/tmux/group.go), so a crash leaves a
//     private session that dies with its source.
//
// The configured detach key therefore always detaches the client that was
// advertised it, whichever value of detach_keys is in force.
type takeoverRoute struct {
	session string // the shared login/config session being taken over
	group   string // this client's private grouped session
	table   string // this client's key table
	key     string // the detach key, in tmux spelling; "" when tmux has none
	display string // the detach key, as the user types it
}

// newTakeoverRoute is the only place a takeover's routing is computed.
func newTakeoverRoute(session string, detachByte byte, display string) takeoverRoute {
	var id [4]byte
	_, _ = rand.Read(id[:])
	group := "af-takeover-" + hex.EncodeToString(id[:])
	r := takeoverRoute{session: session, group: group, table: "af-detach-" + group, display: display}
	r.key, _ = tmux.DetachKeyTmuxName(detachByte)
	return r
}

// prefix is the prefix the takeover's client runs with: C-b, or none when the
// detach key is C-b itself.
func (r takeoverRoute) prefix() string {
	if r.key == "C-b" {
		return "None"
	}
	return "C-b"
}

// attachArgs is the tmux command sequence: create the private session, route its
// keys, say which key leaves (its status line), then attach. A byte tmux has no ctrl
// spelling for degrades to the plain attach to the shared session.
func (r takeoverRoute) attachArgs() []string {
	if r.key == "" {
		return []string{"attach-session", "-t", r.session}
	}
	// Everything is configured BEFORE the attach: commands queued after an
	// attaching command do not run until that client has gone, so a hint or a
	// binding placed after it would arrive too late (the TUI driver selftest
	// caught exactly that). The has-session guard keeps the previous behaviour for
	// a login that exited between the daemon's answer and this start: `new-session
	// -t` accepts a missing target and would create an unrelated shell, where
	// `attach-session -t` failed, so a vanished session is an error again. The
	// private session is created detached. It is NOT given destroy-unattached: that
	// reaper intermittently took the whole tmux server down (#6193).
	return []string{
		"has-session", "-t", "=" + r.session, ";",
		"new-session", "-d", "-t", r.session, "-s", r.group, ";",
		"run-shell", r.mirrorSessionOptionsScript(), ";",
		"run-shell", r.mirrorRootScript(), ";",
		// Every routing option comes AFTER the mirrors (#6221): a source session
		// with its own prefix, prefix2, key-table or status-right must not win.
		"set-option", "-t", r.group, "prefix", r.prefix(), ";",
		"set-option", "-t", r.group, "prefix2", "None", ";",
		"set-option", "-t", r.group, "key-table", r.table, ";",
		"bind-key", "-T", r.table, r.key, "detach-client", ";",
		"set-option", "-t", r.group, "status-right", r.display + " leaves", ";",
		"attach-session", "-t", r.group,
	}
}

// mirrorRootScript copies the root table into this client's table, run by the
// server before the table is selected (#6073). A session's key-table REPLACES root
// with no fallback, and root is where the session's normal pane keeps its mouse
// wheel (scroll into history) and drag (selection) bindings; a table holding only
// the detach key would silently kill them in the takeover pane. `list-keys -T
// root` prints valid `bind-key -T root …` commands, so the copy is a retarget of
// that listing, replayed into the server. The detach binding is added AFTER it, so
// the configured key wins over a root binding on the same key. A failed copy must
// not abort the takeover (the detach route still works), hence `|| true`.
func (r takeoverRoute) mirrorRootScript() string {
	return "tmux list-keys -T root | sed -E 's/^bind-key( +-r)? +-T root +/bind-key\\1 -T " + r.table + " /' | tmux source-file - || true"
}

// mirrorSessionOptionsScript copies the shared session's locally set options onto
// the client's private session, run by the server right after the private session
// is created (#6073). A grouped session shares windows but starts from the
// server's GLOBAL options, so anything af set on the source (`mouse on` for
// scrolling and selection, history-limit, the status line…) would otherwise be
// gone in the takeover pane, and with the global mouse option off the pane loses
// scrolling and selection. `show-options` prints only options set on that session,
// in a form `source-file` accepts. It runs BEFORE every routing option in attachArgs
// (prefix, prefix2, key-table, status-right), so the takeover's own values win
// over a copied one. A failed copy must not abort the takeover.
func (r takeoverRoute) mirrorSessionOptionsScript() string {
	return "tmux show-options -t " + config.ShellQuotePath("="+r.session) + " | sed -E 's/^/set-option -t " + r.group + " /' | tmux source-file - || true"
}

// cleanupArgs are the commands that retire this client's routing: its own table
// and its own private session, never the shared one.
func (r takeoverRoute) cleanupArgs() [][]string {
	if r.key == "" {
		return nil
	}
	return [][]string{
		{"unbind-key", "-a", "-T", r.table},
		{"kill-session", "-t", r.group},
	}
}

var takeoverRoutes sync.Map // *exec.Cmd -> registeredRoute

type registeredRoute struct {
	socket string
	route  takeoverRoute
}

func registerTakeoverRoute(cmd *exec.Cmd, socket string, r takeoverRoute) {
	takeoverRoutes.Store(cmd, registeredRoute{socket: socket, route: r})
}

// bareAttachCleanup retires the routing the takeover command installed, once its
// client is gone. Best-effort: a crash skips it, which is harmless — the table is
// reachable only through the private session, and the daemon kills that session
// with its source.
func bareAttachCleanup(cmd *exec.Cmd) {
	v, ok := takeoverRoutes.LoadAndDelete(cmd)
	if !ok {
		return
	}
	reg := v.(registeredRoute)
	for _, args := range reg.route.cleanupArgs() {
		full := make([]string, 0, len(args)+2)
		if reg.socket != "" {
			full = append(full, "-S", reg.socket)
		}
		c := exec.Command("tmux", append(full, args...)...)
		c.Env = configAgentAttachEnv()
		_ = runBareAttachCleanup(c)
	}
}

// runBareAttachCleanup runs one cleanup command; a var so tests need no tmux.
var runBareAttachCleanup = func(cmd *exec.Cmd) error { return cmd.Run() }
