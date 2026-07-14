# Running tests safely on a shared box

`af`'s test suite and play-tests drive **real tmux servers and real `af`
daemons**. The tests themselves are hermetic (every tmux-touching package
runs on a private tmux server via `internal/testguard`, so plain
`go test ./...` is safe on a normal machine and in CI) — but on a shared
dev box where a real `af` daemon and other people's tmux sessions are
running, one escaped process or one environment mistake is an outage.
This repo ships a container harness that makes that whole class
structurally impossible: the suite and play-tests run inside docker (or
podman) with **no access to the host tmux server, the real
`~/.agent-factory`, or the repo checkout** (mounted read-only).

Requirements: docker or podman on `PATH`. Everything else lives in the
image (`scripts/container/Dockerfile.test`: golang 1.25 + tmux + git).

## `make test-container` — the full suite

```bash
make test-container
# narrow or extend the run:
make test-container GOTESTARGS="-race ./daemon/..."
```

Runs `go test -count=1 ./...` inside the container. This is the **one
sanctioned way to run the bare full suite on a shared box** — on the host,
skip the daemon package (`go test $(go list ./... | grep -v '/daemon')`)
or use this target.

What the harness does:

- mounts the repo **read-only** at `/src` and copies it to a writable tree
  inside the container before running, so no test can modify the checkout;
- caches modules and build artifacts in named volumes
  (`af-testbox-gomod`, `af-testbox-gobuild`) so the second run is fast;
- if the host has a warm Go module cache, exposes it as a read-only
  `file://` module proxy, so runs need no network (`go.sum` still verifies
  every module);
- caps pids and memory (`AF_TESTBOX_PIDS`, `AF_TESTBOX_MEMORY`) so a
  runaway process generator suffocates inside the container instead of
  taking the box down.

## `make remote-roundtrip-container` — mock remote hooks

```bash
make remote-roundtrip-container
```

Runs the focused remote-hook round-trip inside the testbox (#1592 Phase 4 PR7).
The test builds `af`, configures a repo `backend=hook` with a mock `launch_cmd`
that clones the workspace and starts a REAL `af agent-server` on the host, then
drives the migrated provision-and-expose path end to end: create (launch_cmd
echoes the agent-server's `{url,token}`) → the daemon drives it
over `http://`/`ws://` → Subscribe + typed Input echoes back over the stream → Preview/
Snapshot/Alive reflect the pane → Kill runs `delete_cmd`, which reaps the
agent-server (no leak). It does not need Docker-in-Docker; the agent-server runs
as a host subprocess inside the already-isolated test container (needs git + tmux).

## `make playtest-container` — TUI play-testing

```bash
make playtest-container
```

Builds `af` from your checkout inside the container and drops you into a
shell with a ready-made sandbox: a throwaway `AGENT_FACTORY_HOME` (with
`program_overrides` so instances run `bash`, not real agents), a small
mock project repo to drive sessions against, and the container's own tmux
server — `tmux kill-server` in there is harmless. Exiting the shell tears
down everything: the daemon, the tmux server, and every process the
play-test spawned. Teardown is container exit, not a checklist.

For scripted/agent-driven play-tests (the `tui-playtest` skill), park the
sandbox in the background and drive it with `docker exec`. The container name
defaults to a **unique per-run value** (#1171) so concurrent runs can't
`docker rm -f` each other — pin it with `AF_PLAYTEST_NAME` so every
`docker exec`/`rm` targets this run's container:

```bash
export AF_PLAYTEST_NAME="af-playtest-$$"
make playtest-container-detached
docker exec "$AF_PLAYTEST_NAME" sh -c 'until [ -x /home/dev/bin/af ]; do sleep 1; done'
docker exec "$AF_PLAYTEST_NAME" tmux new-session -d -s drive -x 80 -y 24
docker exec "$AF_PLAYTEST_NAME" tmux send-keys -t drive 'cd ~/sandbox/mock-repo && af' Enter
docker exec "$AF_PLAYTEST_NAME" tmux capture-pane -p -t drive
docker rm -f "$AF_PLAYTEST_NAME"   # teardown: one command reaps everything
```

Rather than hand-rolling `send-keys`/`capture-pane`/`sleep`, drive the TUI
through the **deterministic driver** (`scripts/tui-driver.sh`): every action
waits on a screen marker instead of a blind sleep, and it ships real
assertions. `make tui-driver-selftest` is the acceptance gate; `make
tui-driver` drops you into a live driven session. See
[tui-manual-testing.md](tui-manual-testing.md).

## Known limitations

- **No systemd inside** — autostart-unit flows (`af daemon install`) still
  need CI or careful host testing.
- **Network-dependent flows** (`gh`, pushing branches) need an explicitly
  injected token; the sandbox has none by default, which keeps external
  access opt-in and visible.
- The first image build downloads the golang base image (a few minutes);
  every run after that reuses cached layers.

## Where the container is NOT the answer

CI runs the suite bare on GitHub runners (already isolated) and external
contributors will always run plain `go test ./...` — both fine, because
the in-tree isolation (`internal/testguard`, #1122/#1125) makes the tests
themselves safe. The container is the belt-and-suspenders outer wall for
shared dev boxes, not a substitute for hermetic tests.
