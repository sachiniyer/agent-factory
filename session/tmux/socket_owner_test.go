package tmux

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The verdict over the sweep's own buckets (#7695). Each row hands
// socketClaimedBySweep a constructed census — the shapes the kernel-table
// sweep produces that the candidacy table cannot: undecidable bound
// spellings, bound rows whose holder no pid could be named for, live pids
// whose netns went uncovered — and pins the answer, who is signalled, and
// whether the watch ran. A row the probe should answer without watching
// runs with a 30 s window and must return in well under it.
func TestSocketClaimedBySweepDecisionTable(t *testing.T) {
	const (
		server    = 9501 // positively a tmux server
		server2   = 9502
		notServer = 9503 // bound to the path but not positively a server
		covered   = 9504 // a live pid the negative could not be proved for
	)
	bound := func(pids ...int) map[int]boundOwner {
		m := map[int]boundOwner{}
		for _, p := range pids {
			m[p] = boundOwner{spelling: "/bound/path", source: "test"}
		}
		return m
	}
	for _, tc := range []struct {
		name        string
		sweep       socketSweep
		killErr     map[int]error
		recreatedBy int // the owner whose signal recreates the socket
		socketThere bool
		wantClaimed bool
		wantSignal  []int
		watches     bool
	}{
		{name: "nothing bound, nobody uncovered: unclaimed", sweep: socketSweep{}},
		{name: "the sweep could not run: claimed", sweep: socketSweep{unprobed: true}, wantClaimed: true},
		{name: "a live pid the negative cannot be proved for: claimed", sweep: socketSweep{uncovered: []int{covered}}, wantClaimed: true},
		{name: "an undecidable bound spelling: claimed, never signalled", sweep: socketSweep{uncertain: map[int]string{server: "/uncertain"}}, wantClaimed: true},
		{name: "a bound row no holder could be named for: claimed", sweep: socketSweep{unattributedBound: 1}, wantClaimed: true},
		{name: "an undecidable row no holder could be named for: claimed", sweep: socketSweep{unattributedUncertain: 1}, wantClaimed: true},
		{name: "bound, but not positively a server: claimed, never signalled", sweep: socketSweep{bound: bound(notServer)}, wantClaimed: true},
		{name: "the owner recreates the socket: claimed", sweep: socketSweep{bound: bound(server)}, recreatedBy: server, wantClaimed: true, wantSignal: []int{server}, watches: true},
		{name: "the owner leaves it absent: unclaimed", sweep: socketSweep{bound: bound(server)}, wantSignal: []int{server}, watches: true},
		{name: "the owner refuses the signal (EPERM): claimed", sweep: socketSweep{bound: bound(server)}, killErr: map[int]error{server: syscall.EPERM}, wantClaimed: true, wantSignal: []int{server}},
		{name: "the owner is gone before the signal (ESRCH): unclaimed", sweep: socketSweep{bound: bound(server)}, killErr: map[int]error{server: syscall.ESRCH}, wantSignal: []int{server}},
		{name: "a signalled owner beside an unattributed holder: claimed", sweep: socketSweep{bound: bound(server), unattributedBound: 1}, wantClaimed: true, wantSignal: []int{server}, watches: true},
		{name: "a signalled owner beside an uncovered pid: claimed", sweep: socketSweep{bound: bound(server), uncovered: []int{covered}}, wantClaimed: true, wantSignal: []int{server}, watches: true},
		{name: "two owners, one refuses: claimed", sweep: socketSweep{bound: bound(server, server2)}, killErr: map[int]error{server2: syscall.EACCES}, wantClaimed: true, wantSignal: []int{server, server2}, watches: true},
		{name: "nothing bound but the socket is back: claimed", sweep: socketSweep{}, socketThere: true, wantClaimed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "default")
			if tc.socketThere {
				require.NoError(t, os.WriteFile(socket, nil, 0o600))
			}
			pinServerIdentityForTest(t, server, server2, covered)
			var signalled []int
			pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
				require.Equal(t, syscall.SIGUSR1, sig)
				signalled = append(signalled, pid)
				if err := tc.killErr[pid]; err != nil {
					return err
				}
				if pid == tc.recreatedBy {
					require.NoError(t, os.WriteFile(socket, nil, 0o600))
				}
				return nil
			})
			window := 50 * time.Millisecond
			if !tc.watches {
				window = 30 * time.Second
			}
			pinReclaimWindowForTest(t, window)

			start := time.Now()
			require.Equal(t, tc.wantClaimed, socketClaimedBySweep(socket, tc.sweep))
			require.Equal(t, tc.wantSignal, signalled, "only a bound pid positively identified as a server is signalled")
			if !tc.watches {
				require.Less(t, time.Since(start), 10*time.Second, "nothing was left to watch, so the answer comes at once")
			}
		})
	}
}

// possibleSocketOwners splits one sweep the refusal's way: identified
// servers apart from possible owners it may not signal, with the
// unattributed count kept whole — a bound holder af cannot name is a
// refusal reason of its own (#7695).
func TestPossibleSocketOwnersBySweep(t *testing.T) {
	const server, notServer, covered = 9601, 9602, 9603
	pinServerIdentityForTest(t, server, covered)
	servers, unidentified, unattributed := possibleSocketOwnersBySweep(socketSweep{
		bound:                 map[int]boundOwner{server: {spelling: "/b"}, notServer: {spelling: "/b"}},
		uncertain:             map[int]string{covered: "/u"},
		uncovered:             []int{covered},
		unattributedBound:     1,
		unattributedUncertain: 2,
	})
	require.Equal(t, []int{server, covered}, servers, "identified possible owners, bound or undecidable")
	require.Equal(t, []int{notServer}, unidentified)
	require.Equal(t, 3, unattributed, "certain and uncertain unnamed holders both count")

	servers, unidentified, unattributed = possibleSocketOwnersBySweep(socketSweep{unprobed: true})
	require.Empty(t, servers)
	require.Equal(t, []int{0}, unidentified, "the unreadable-sweep sentinel is a possible owner")
	require.Zero(t, unattributed)

	servers, unidentified, unattributed = possibleSocketOwnersBySweep(socketSweep{})
	require.Empty(t, servers)
	require.Empty(t, unidentified)
	require.Zero(t, unattributed)
}
