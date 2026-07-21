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
  taking the box down;
- cleans up after itself on the way out, pass or fail, so repeated runs don't
  grow `/var/lib/docker` — see [disk footprint](#disk-footprint).

## Disk footprint

This harness once filled a 2TB dev box (#2133). `/` hit 100%, and the first
thing to break was not the tests — it was the running `af` daemon, which
shares the filesystem and could no longer persist session state. So every
target here is now **self-limiting on disk**, and none of the cleanup can
reach anything the harness did not create.

Every image, container, and cache volume the harness makes carries the label
`af.harness=testbox` — the same idea as the `af.session` label the docker
backend puts on session containers. Cleanup filters on it, so a prune here can
only ever match the harness's own artifacts.

**On every run**, on the way out — including when the suite *fails*, which is
exactly the run you re-run and therefore the one that used to compound fastest:

- containers are `--rm`, so no writable layer and no anonymous volume survives;
- images this harness built that no longer carry a tag are pruned. Every target
  builds a *stable* tag, so editing a Dockerfile — or just running from a
  sibling worktree whose Dockerfile differs — moves the tag off the previous
  image (~1.2GB for the testbox image, ~4GB for web-selftest). Whether that
  image then lingers depends on the engine: the classic `overlay2` image store
  leaves it as a dangling `<none>` forever, while the containerd snapshotter
  (docker 29's default) collects it itself. The reap costs nothing on the
  engines that don't need it;
- the shared BuildKit cache is held under a ceiling (default 10GB,
  `AF_TESTBOX_CACHE_MAX`). This is the one cleanup that cannot be
  label-scoped — BuildKit cache records carry no labels — so it is deliberately
  a *ceiling* and not a wipe: docker evicts least-recently-used records until
  the total fits, which leaves another builder's warm cache alone as long as it
  is warmer than ours, and anything evicted is rebuildable. Set
  `AF_TESTBOX_CACHE_MAX=off` to skip it entirely.

The net effect: images, containers and build cache cost about as much for N
runs as for one. The Go cache volumes are the honest exception — they keep
growing with how much you build, bounded only by Go's own 5-day cache trim,
which is why `make testbox-clean` exists.

**When you want the space back**, including the Go caches:

```bash
make testbox-clean
```

That removes the harness's stopped containers and images, empties the four
named cache volumes (`af-testbox-{gomod,gobuild}`,
`af-web-selftest-{gomod,gobuild}`), caps the build cache, and prints
`docker system df`. The Go build cache volume is the largest thing here — tens
of GB on a busy box — and no automatic step touches it, because emptying it
costs the next run a full cold rebuild. Go trims its own cache at 5 days, so it
is bounded, just generously.

`make testbox-clean` reports **running** containers rather than removing them
and prints the exact `docker rm -f` for each. On a box with several worktrees a
running labelled container is most likely a sibling's in-flight suite or a
parked play-test sandbox, and a cleanup target is not allowed to be the thing
that kills it.

Nothing here ever runs `docker system prune` or an unfiltered
`docker volume prune`. Both would have fixed the disk and deleted co-tenants'
images to do it. `make testbox-selftest` asserts exactly that, against a fake
docker, in about a second — it gates every PR.

Tagged-image cleanup is also serialized against the only unsafe window in a
sibling run: from rebuilding a stable harness tag until Docker/Podman reports
the first container using it as running. The lock is shared across worktrees and
released at that positive engine observation, so suites still run concurrently;
afterward the container reference itself prevents image deletion. This keeps
`image prune -a` and the legacy exact-tag cleanup fallback from deleting a local-
only image between build and `run`, and also keeps two sibling Dockerfile builds
from retagging the name out from under a not-yet-created container.

**Before a run**, if free space on the docker root filesystem is under 20GB
(`AF_TESTBOX_MIN_FREE_GB`, `0` silences it), the harness says so and points at
`make testbox-clean`. It warns and continues rather than refusing: nothing here
knows how much room a given run needs, and a harness that won't start is its
own outage. Every way of *not* being able to tell — a remote engine, an
unreadable root — stays quiet instead of guessing.

### What actually accumulates

Measured on the box that filled up, so the next person doesn't have to re-derive
it. Categories, largest first:

| Category | Bounded by | Notes |
| --- | --- | --- |
| Cache volumes (~30GB) | Go's own 5-day cache trim; `make testbox-clean` | The largest item by far, and `docker system prune -af` does **not** touch volumes |
| Build cache (~5GB) | the per-run ceiling above | Nothing pruned it before |
| Harness images (~5GB) | the per-run reap above | Only strands copies on `overlay2`; see above |
| Containers | `--rm` | Never a leak; `--rm` was always there |

One correction to the incident write-up in #2133 while we're here. The reclaim
that recovered the box was `docker system prune -af` **plus** clearing the
host's `~/.cache/go-build`, and the "post-prune `/var/lib/docker` was 4.0K"
reading that the write-up took as "docker held essentially all of it" was a
`du` without root — `/var/lib/docker` is `drwx--x---  root root`, so a non-root
`du` prints a permission error on stderr and `4.0K` on stdout. Docker did not
hold all 312GB; the host Go build cache held a share of it that this reading
could not see. Both are regenerable, and only the docker half is this harness's
to bound — but if this box fills again, check `~/.cache/go-build` too.

> One-time note for boxes that ran the harness before this landed: images built
> then carry no label, so the label-scoped reap cannot see them. Clear that
> backlog once with `docker image prune -f` (dangling images only).

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

## `make lifecycle-container` — clean install + upgrade

```bash
make lifecycle-container
make lifecycle-container LIFECYCLE_SCENARIO=scenario-a   # narrow it
```

The one target here that does **not** just test this tree: it installs a REAL
previous release into a throwaway machine, puts sessions on it, upgrades it the
way a user does (`af upgrade` and the launch-time auto-update), and asserts the
machine is coherent afterwards — daemon restarted onto the new binary, no
client/daemon version skew, exactly one daemon, sessions survived, `af doctor`
clean. That version boundary is where #1921 and #796 shipped, and no
single-version test can construct it.

Two things make it unlike the other targets: it needs **network** (it downloads
published release tarballs), so it is wired **nightly** rather than per-PR; and
it **cannot** cover the autostart-supervision assertion here, because there is no
systemd in the container — that assertion is SKIPped loudly and covered by the
CI runner leg. See [lifecycle-testing.md](lifecycle-testing.md).

## Known limitations

- **No systemd inside** — autostart-unit flows (`af daemon install`) still
  need CI or careful host testing. It fails outright in the container
  ("systemctl: executable file not found"), which is why the lifecycle gate
  SKIPs its supervision assertion here and runs it on the CI runner instead.
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
