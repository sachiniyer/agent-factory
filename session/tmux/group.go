package tmux

import "strings"

// groupMateSessions returns the names of the OTHER sessions in name's tmux session
// group. A grouped session shares its windows with the rest of its group, so
// `kill-session` on one member closes the windows only for sessions that are not
// linked elsewhere ("and no other sessions"): while a mate survives, so does the
// window, and the process in it. af creates such mates itself — a takeover client
// attaches its own private grouped session of a daemon-owned login or config
// session (#6078) — so teardown of the source session has to take them with it,
// synchronously, instead of leaving a process alive behind a session nobody
// supervises.
//
// Best-effort and bounded: a session that is in no group, a server that does not
// answer, or output that cannot be read all return nil, and the caller's own
// kill-session proceeds exactly as before.
func (t *TmuxSession) groupMateSessions() []string {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, t.cmdExec, "list-sessions", "-F", "#{session_group}\t#{session_name}")
	if err != nil {
		return nil
	}
	return groupMatesFromListing(string(out), t.sanitizedName)
}

// groupMatesFromListing parses `#{session_group}\t#{session_name}` rows.
func groupMatesFromListing(listing, name string) []string {
	type row struct{ group, name string }
	var rows []row
	group := ""
	for _, line := range strings.Split(listing, "\n") {
		g, n, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok || n == "" {
			continue
		}
		rows = append(rows, row{g, n})
		if n == name {
			group = g
		}
	}
	if group == "" {
		return nil
	}
	var mates []string
	for _, r := range rows {
		if r.group == group && r.name != name {
			mates = append(mates, r.name)
		}
	}
	return mates
}
