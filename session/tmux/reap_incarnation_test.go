package tmux

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// fakeIncarnation is one tmux session as a server holds it.
type fakeIncarnation struct {
	id, name, pid, created string
	env                    map[string]string
}

// fakeTmuxServer answers the commands ReapProvenIncarnation issues the way a
// tmux server does: a target is either `=name:` (the session called name) or
// `$N` (the session with that id), a missing session exits 1 with "can't find
// session: <target>", and a missing variable exits 1 with "unknown variable".
// It records every argv so a test asserts what was and was not run.
type fakeTmuxServer struct {
	t        *testing.T
	mu       sync.Mutex
	sessions []*fakeIncarnation
	commands []string
	// after, when set, runs after every command whose argv contains the key — the
	// hook that replaces a session between two commands of one reap.
	after map[string]func(*fakeTmuxServer)
	// unreadableEnv makes every show-environment fail with a non-"unknown
	// variable" error, the shape of an environment that cannot be read.
	unreadableEnv bool
	// delay makes every command outlast tmuxCommandTimeout.
	delay time.Duration
	// killFails makes kill-session fail with an error that says nothing about the
	// session being gone, and oddHasSession does the same for has-session: the
	// shapes of a kill that did not take and a probe that cannot tell.
	killFails, oddHasSession bool
	// executedKills are the kill-session commands the server actually RAN, whether
	// sent directly or as the branch of a guarded if-shell. A guarded kill whose
	// condition failed is not in here — that is the whole point of the guard.
	executedKills []string
}

func exitOneWith(t *testing.T, stderr string) error {
	t.Helper()
	_, err := exec.Command("sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", stderr).Output()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	return err
}

func (s *fakeTmuxServer) find(target string) *fakeIncarnation {
	for _, sess := range s.sessions {
		if strings.HasPrefix(target, "$") && sess.id == target {
			return sess
		}
		if target == "="+sess.name+":" || target == "="+sess.name {
			return sess
		}
	}
	return nil
}

func fakeTargetOf(args []string) string {
	for i, a := range args {
		if a == "-t" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "-t=") {
			return "=" + strings.TrimPrefix(a, "-t=")
		}
	}
	return ""
}

func (s *fakeTmuxServer) answer(c *exec.Cmd) ([]byte, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
		return nil, fmt.Errorf("tmux did not answer in time")
	}
	args := c.Args[1:]
	line := strings.Join(args, " ")
	s.mu.Lock()
	s.commands = append(s.commands, line)
	var hook func(*fakeTmuxServer)
	for key, h := range s.after {
		if strings.Contains(line, key) {
			hook = h
		}
	}
	out, err := s.answerLocked(args)
	s.mu.Unlock()
	if hook != nil {
		s.mu.Lock()
		hook(s)
		s.mu.Unlock()
	}
	return out, err
}

func (s *fakeTmuxServer) answerLocked(args []string) ([]byte, error) {
	target := fakeTargetOf(args)
	sess := s.find(target)
	// tmux words a vanished session per command: measured on the CI runner,
	// show-environment says "no such session" where kill-session says "can't find
	// session". The fake keeps that split so a classifier that learned one wording
	// cannot pass here.
	bare := strings.TrimSuffix(strings.TrimPrefix(target, "="), ":")
	missing := exitOneWith(s.t, "can't find session: "+bare)
	if args[0] == "show-environment" {
		missing = exitOneWith(s.t, "no such session: "+bare)
	}
	switch args[0] {
	case "display-message":
		if sess == nil {
			// Measured on tmux 3.4: aimed at a session that does not exist,
			// display-message SUCCEEDS, evaluating the format in a session-less
			// fallback context — "#{pid}" still answers, "#{session_id}" is empty.
			return []byte("114183\n"), nil
		}
		format := args[len(args)-1]
		if strings.HasPrefix(format, "af-reap-") {
			return []byte(format + "\n"), nil
		}
		switch format {
		case "#{session_id} #{pid} #{session_created}":
			return []byte(sess.id + " " + sess.pid + " " + sess.created + "\n"), nil
		case "#{pid} #{session_created}":
			return []byte(sess.pid + " " + sess.created + "\n"), nil
		}
		return nil, fmt.Errorf("fake tmux: unexpected display-message format %q", format)
	case "show-environment":
		if sess == nil {
			return nil, missing
		}
		if s.unreadableEnv {
			return nil, exitOneWith(s.t, "protocol version mismatch")
		}
		if len(args) < 4 {
			return nil, nil
		}
		name := args[3]
		if v, ok := sess.env[name]; ok {
			return []byte(name + "=" + v + "\n"), nil
		}
		return nil, exitOneWith(s.t, "unknown variable: "+name)
	case "list-panes":
		if sess == nil {
			return nil, missing
		}
		// One pane row, prefixed with the pid and creation stamp the server
		// evaluates while answering — exactly what the real format does.
		return []byte(sess.id + "|" + sess.pid + "|" + sess.created + "|5555|0|||||\n"), nil
	case "has-session":
		if s.oddHasSession {
			return nil, exitOneWith(s.t, "unexpected failure")
		}
		if sess == nil {
			return nil, missing
		}
		return nil, nil
	case "kill-session":
		if s.killFails {
			return nil, exitOneWith(s.t, "operation not permitted")
		}
		if sess == nil {
			return nil, missing
		}
		s.executedKills = append(s.executedKills, "kill-session -t "+target)
		for i, other := range s.sessions {
			if other == sess {
				s.sessions = append(s.sessions[:i], s.sessions[i+1:]...)
				break
			}
		}
		return nil, nil
	case "if-shell":
		// if-shell -F -t T COND TRUE FALSE: the server evaluates COND against the
		// target and runs exactly one branch, as one command.
		if sess == nil {
			return nil, missing
		}
		cond, trueBranch, falseBranch := args[4], args[5], args[6]
		m := regexp.MustCompile(`^#\{&&:#\{==:#\{session_id\},(\$\d+)\},#\{&&:#\{==:#\{pid\},(\d+)\},#\{==:#\{session_created\},(\d+)\}\}\}$`).FindStringSubmatch(cond)
		if m == nil {
			return nil, fmt.Errorf("fake tmux: unexpected if-shell condition %q", cond)
		}
		branch := falseBranch
		if m[1] == sess.id && m[2] == sess.pid && m[3] == sess.created {
			branch = trueBranch
		}
		var out []byte
		for _, command := range strings.Split(branch, " ; ") {
			words := strings.Fields(strings.ReplaceAll(command, "'", ""))
			part, err := s.answerLocked(words)
			if err != nil {
				return nil, err
			}
			out = append(out, part...)
		}
		return out, nil
	case "ls":
		// The server's own listing, which corroborates a failure that names
		// neither the id nor a missing server.
		var b strings.Builder
		for _, other := range s.sessions {
			if args[len(args)-1] == "#{session_id}" {
				b.WriteString(other.id + "\n")
			} else {
				b.WriteString(other.name + "\n")
			}
		}
		return []byte(b.String()), nil
	}
	return nil, fmt.Errorf("fake tmux: unexpected command %q", strings.Join(args, " "))
}

func (s *fakeTmuxServer) executor() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			_, err := s.answer(c)
			return err
		},
		OutputFunc: s.answer,
	}
}

// kills are the kill-session commands the server executed.
func (s *fakeTmuxServer) kills() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.executedKills...)
}

// guardedKills are the if-shell commands sent: every kill goes through one.
func (s *fakeTmuxServer) guardedKills() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.commands {
		if strings.HasPrefix(c, "if-shell") {
			out = append(out, c)
		}
	}
	return out
}

func (s *fakeTmuxServer) has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find("="+name+":") != nil
}

const (
	reapName     = "af_repo_fire"
	reapHome     = "/home/u/.agent-factory"
	reapInstance = "inst-6258-a"
)

func newFakeTmuxServer(t *testing.T, env map[string]string) *fakeTmuxServer {
	t.Helper()
	return &fakeTmuxServer{t: t, sessions: []*fakeIncarnation{{
		id: "$7", name: reapName, pid: "4242", created: "1700000000", env: env,
	}}}
}

func provenEnv() map[string]string {
	return map[string]string{
		EnvMarkerSession:  reapName,
		EnvMarkerHome:     reapHome,
		EnvMarkerInstance: reapInstance,
	}
}

func reapProof() IncarnationProof {
	return IncarnationProof{SessionName: reapName, Home: reapHome, InstanceID: reapInstance}
}

// stubIncarnationProcesses replaces the two seams that need a real tmux server and
// real processes: the pane-row parse returns one fake captured process, and the
// reap records what it was asked to signal and how many kills had executed by then.
type reapRecord struct {
	pids          []int
	killsAtReap   int
	reapCallCount int
}

func stubIncarnationProcesses(t *testing.T, srv *fakeTmuxServer) *reapRecord {
	t.Helper()
	rec := &reapRecord{}
	prevParse, prevReap := incarnationPaneProcesses, incarnationReapProcesses
	incarnationPaneProcesses = func(rows, name string) ([]proctree.Process, error) {
		return []proctree.Process{{PID: 777001}}, nil
	}
	incarnationReapProcesses = func(_ reapReason, _ string, procs []proctree.Process, _, _ time.Duration) []proctree.Process {
		rec.reapCallCount++
		rec.killsAtReap = len(srv.kills())
		for _, p := range procs {
			rec.pids = append(rec.pids, p.PID)
		}
		return nil
	}
	t.Cleanup(func() { incarnationPaneProcesses, incarnationReapProcesses = prevParse, prevReap })
	return rec
}

// #6258: a session whose own environment carries this fire's instance id is
// stopped by ONE guarded server-side command that re-checks the server pid and
// creation stamp, and the processes captured from that incarnation are reaped only
// after the kill ran.
func TestReapProvenIncarnationKillsTheProbedIdOnAMatch(t *testing.T) {
	srv := newFakeTmuxServer(t, provenEnv())
	rec := stubIncarnationProcesses(t, srv)
	got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

	require.True(t, got.Stopped, "why: %s", got.Why)
	require.Equal(t, []string{"kill-session -t $7"}, srv.kills(),
		"the kill must target the probed incarnation id, and nothing else")
	guarded := srv.guardedKills()
	require.Len(t, guarded, 1, "the kill must be exactly one guarded server-side command")
	require.Contains(t, guarded[0], "if-shell -F -t $7 #{&&:#{==:#{session_id},$7},#{&&:#{==:#{pid},4242},#{==:#{session_created},1700000000}}}",
		"the guard must compare the session id, server pid and creation stamp in the server")
	require.False(t, srv.has(reapName))
	require.Equal(t, 1, rec.reapCallCount)
	require.Equal(t, []int{777001}, rec.pids)
	require.Equal(t, 1, rec.killsAtReap, "the captured processes are reaped only after the kill fired")
}

// The property of #6260's review: the destructive operation runs only if, when the
// SERVER executes it, the target is still the verified incarnation. A server that
// is replaced between the client's last check and the kill — and reissues "$7" —
// is not killed, and nothing captured is reaped.
func TestReapProvenIncarnationDoesNothingWhenTheServerChangesBeforeTheKill(t *testing.T) {
	replaceServer := func(s *fakeTmuxServer) {
		s.sessions[0].pid = "9999"
		s.sessions[0].created = "1700000555"
	}
	cases := map[string]string{
		// After the client's re-confirmation: only the guard can catch it.
		"after the re-confirmation": "display-message -p -t $7 #{session_id} #{pid} #{session_created}",
		// After the pane capture, immediately before the kill.
		"after the pane capture": "list-panes -s -t $7",
	}
	for name, trigger := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newFakeTmuxServer(t, provenEnv())
			rec := stubIncarnationProcesses(t, srv)
			srv.after = map[string]func(*fakeTmuxServer){trigger: replaceServer}
			got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

			require.False(t, got.Stopped)
			require.Contains(t, got.Why, "replaced")
			require.Empty(t, srv.kills(), "the guard must stop the kill on a replaced server")
			require.True(t, srv.has(reapName), "the session at the reissued id survives")
			require.Zero(t, rec.reapCallCount, "nothing captured may be reaped when the kill did not fire")
			require.Len(t, srv.guardedKills(), 1, "the guard itself was evaluated by the server")
		})
	}
}

// Each of these leaves the session ALONE, asserted on the recorded argv: no
// kill-session of any target was issued.
func TestReapProvenIncarnationLeavesUnprovenSessionsAlone(t *testing.T) {
	without := func(drop string) map[string]string {
		env := provenEnv()
		delete(env, drop)
		return env
	}
	with := func(key, value string) map[string]string {
		env := provenEnv()
		env[key] = value
		return env
	}
	cases := []struct {
		name string
		env  map[string]string
		mod  func(*fakeTmuxServer)
	}{
		{name: "a different instance id (a concurrent create, same name and home)", env: with(EnvMarkerInstance, "inst-someone-else")},
		{name: "no instance id (a session from before the marker existed)", env: without(EnvMarkerInstance)},
		{name: "a different install's home", env: with(EnvMarkerHome, "/other/home")},
		{name: "no home marker", env: without(EnvMarkerHome)},
		{name: "a different AF_SESSION", env: with(EnvMarkerSession, "af_other")},
		{name: "an environment that cannot be read", env: provenEnv(), mod: func(s *fakeTmuxServer) { s.unreadableEnv = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeTmuxServer(t, tc.env)
			if tc.mod != nil {
				tc.mod(srv)
			}
			got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

			require.False(t, got.Stopped)
			require.NotEmpty(t, got.Why, "a refusal must say which condition kept the session alone")
			require.Empty(t, srv.kills(), "an unproven session must not be killed, by id or by name")
			require.True(t, srv.has(reapName))
		})
	}
}

func TestReapProvenIncarnationRequiresEveryProofField(t *testing.T) {
	for name, proof := range map[string]IncarnationProof{
		"no instance id (the fire recorded none before its spawn)": {SessionName: reapName, Home: reapHome},
		"no home": {SessionName: reapName, InstanceID: reapInstance},
		"no name": {Home: reapHome, InstanceID: reapInstance},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeTmuxServer(t, provenEnv())
			got := ReapProvenIncarnation(srv.executor(), "claude", proof)

			require.False(t, got.Stopped)
			require.NotEmpty(t, got.Why)
			srv.mu.Lock()
			defer srv.mu.Unlock()
			require.Empty(t, srv.commands, "an incomplete proof must not even probe tmux")
		})
	}
}

// Unknown is never ours: a probe that does not answer leaves the session alone.
func TestReapProvenIncarnationLeavesAnUnansweredProbeAlone(t *testing.T) {
	previous := tmuxCommandTimeout
	tmuxCommandTimeout = 20 * time.Millisecond
	t.Cleanup(func() { tmuxCommandTimeout = previous })

	srv := newFakeTmuxServer(t, provenEnv())
	srv.delay = 120 * time.Millisecond
	got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

	require.False(t, got.Stopped)
	require.NotEmpty(t, got.Why)
	require.Empty(t, srv.kills())
}

// The name is re-created between the probe and the kill. Whatever the interleaving,
// the replacement is never killed.
func TestReapProvenIncarnationNeverKillsASessionRecreatedUnderTheName(t *testing.T) {
	recreate := func(s *fakeTmuxServer) {
		// The probed incarnation dies and another is created under the same name
		// with a new id, carrying the SAME markers a real create would stamp.
		s.sessions = []*fakeIncarnation{{
			id: "$9", name: reapName, pid: "4242", created: "1700000099", env: provenEnv(),
		}}
	}
	cases := map[string]string{
		// After the environment is read: the re-confirmation finds the id gone.
		"after the environment read": "show-environment -t $7 AF_SESSION",
		// After the re-confirmation: the kill itself finds the id gone.
		"after the re-confirmation": "display-message -p -t $7 #{session_id} #{pid} #{session_created}",
	}
	for name, trigger := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newFakeTmuxServer(t, provenEnv())
			srv.after = map[string]func(*fakeTmuxServer){trigger: recreate}
			ReapProvenIncarnation(srv.executor(), "claude", reapProof())

			require.True(t, srv.has(reapName), "the session re-created under the name must survive")
			require.Empty(t, srv.kills(), "nothing may be killed once the probed id is gone")
		})
	}
}

// A replacement server reissues "$7": the id alone is not an identity, the server
// pid and creation stamp are.
func TestReapProvenIncarnationRejectsAServerThatReissuedTheId(t *testing.T) {
	srv := newFakeTmuxServer(t, provenEnv())
	srv.after = map[string]func(*fakeTmuxServer){
		"show-environment -t $7 AF_SESSION": func(s *fakeTmuxServer) {
			s.sessions[0].pid = "9999"
			s.sessions[0].created = "1700000555"
		},
	}
	got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

	require.False(t, got.Stopped)
	require.Contains(t, got.Why, "replaced")
	require.Empty(t, srv.kills())
}

// A vanished id never reads as a match. tmux's display-message does not fail for it —
// it succeeds with EMPTY output — so the identity probe demands #{session_id} in the
// answer and treats an empty reply as unknown. Here the id vanishes before the
// environment read; nothing is killed.
func TestReapProvenIncarnationNeverReadsAnEmptyAnswerAsAMatch(t *testing.T) {
	srv := newFakeTmuxServer(t, provenEnv())
	srv.after = map[string]func(*fakeTmuxServer){
		"display-message -p -t =" + reapName + ": #{session_id} #{pid} #{session_created}": func(s *fakeTmuxServer) {
			s.sessions = nil
		},
	}
	got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

	require.False(t, got.Stopped)
	require.NotEmpty(t, got.Why)
	require.Empty(t, srv.kills())
}

// A kill that fails for a reason that does not say the session is gone, followed by a
// probe that cannot tell, is UNKNOWN: the reap reports it not stopped rather than
// reading "could not ask" as "gone".
func TestReapProvenIncarnationTreatsAnUnclassifiableFailureAsUnknown(t *testing.T) {
	srv := newFakeTmuxServer(t, provenEnv())
	srv.killFails = true
	srv.oddHasSession = true
	got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

	require.False(t, got.Stopped, "an unknown outcome must not be reported as stopped")
	require.NotEmpty(t, got.Why)
	require.True(t, srv.has(reapName), "the session is still there")
}

// The same kill failure with a probe that answers "alive" is a refused kill.
func TestReapProvenIncarnationReportsAKillThatDidNotTake(t *testing.T) {
	srv := newFakeTmuxServer(t, provenEnv())
	srv.killFails = true
	got := ReapProvenIncarnation(srv.executor(), "claude", reapProof())

	require.False(t, got.Stopped)
	require.True(t, srv.has(reapName))
}
