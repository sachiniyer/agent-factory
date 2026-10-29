package app

import (
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"sync"

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
//   - prefix: tmux consults the prefix before the table, so a detach key equal to
//     the prefix would never reach its binding (#6074). The takeover therefore
//     routes the prefix away from the detach key: the prefix is C-b, unless the
//     configured key IS C-b, in which case the prefix is disabled and the detach
//     key wins. prefix2 is always disabled. There is no collision left to refuse.
//   - destroy-unattached: the private session dies with its client, so a crash
//     leaves nothing selectable behind.
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
// keys, attach, and tell the user which key leaves. A byte tmux has no ctrl
// spelling for degrades to the plain attach to the shared session.
func (r takeoverRoute) attachArgs() []string {
	if r.key == "" {
		return []string{"attach-session", "-t", r.session}
	}
	return []string{
		"new-session", "-t", r.session, "-s", r.group, ";",
		"set-option", "-t", r.group, "destroy-unattached", "on", ";",
		"set-option", "-t", r.group, "prefix", r.prefix(), ";",
		"set-option", "-t", r.group, "prefix2", "None", ";",
		"set-option", "-t", r.group, "key-table", r.table, ";",
		"bind-key", "-T", r.table, r.key, "detach-client", ";",
		"display-message", "-d", "4000", r.display + " leaves",
	}
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
// client is gone. Best-effort: a crash skips it, which is harmless — the private
// session was destroy-unattached and its table is reachable only through it.
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
