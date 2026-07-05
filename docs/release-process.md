# Release Process

Agent Factory ships on two channels (#1041):

| Channel | Version shape | Cut by | GitHub release |
| --- | --- | --- | --- |
| **Stable** | `1.x.y` (e.g. `1.1.0`) | Maintainer, manually | Normal release, marked **latest** |
| **Preview** | `1.x.y-preview-z` (e.g. `1.1.1-preview-3`) | CI, automatically every 3 hours | **Prerelease**, never marked latest |

## Version scheme

Previews are based on the **next patch after the latest stable**: if the
latest stable is `1.1.0`, previews are `1.1.1-preview-1`, `1.1.1-preview-2`,
… — each cut from the current `master` HEAD. Because a prerelease sorts
below its own release in semver, this keeps ordering standard across both
channels:

```
1.1.0  <  1.1.1-preview-1  <  1.1.1-preview-2  <  1.1.1  ≤  any newer stable
```

`z` increments on every preview and resets to 1 whenever a new stable
changes the base. Version computation lives in
[`.github/scripts/next-preview-version.sh`](../.github/scripts/next-preview-version.sh)
and is unit-tested in `release_scripts_test.go`.

## Preview releases (automatic)

The [`auto-release.yml`](../.github/workflows/auto-release.yml) workflow runs
every 3 hours (and on manual dispatch). When `master` has new commits since
the last tag, it runs the release preflight (gofmt, vet, race tests, build),
tags `v1.x.y-preview-z`, builds the four platform tarballs with the version
stamped via `-ldflags "-X main.version=..."`, and publishes a GitHub
**prerelease**.

Previews never commit to `master` — the old per-release
"chore: bump version" commits are gone. The `version` var in `main.go` is
only the dev-build fallback and holds the latest stable base.

## Stable releases (manual)

To cut a stable release, run the **Stable Release** workflow
([`stable-release.yml`](../.github/workflows/stable-release.yml)) from the
Actions tab (`workflow_dispatch`) and enter the version with no leading `v`,
e.g. `1.1.0`. The maintainer chooses which digit to bump; the workflow
validates the version (well-formed, untagged, strictly greater than the
latest stable — see
[`.github/scripts/validate-stable-version.sh`](../.github/scripts/validate-stable-version.sh)),
runs the same preflight, and builds all four artifacts **before mutating
anything**; only then does it commit the version bump to `main.go`, tag,
and publish the release marked **latest**. A failed preflight or build
therefore never leaves a dangling commit or tag on `master`.

Releasing the current preview base as-is (e.g. `1.0.138` while
`1.0.138-preview-9` exists) is supported — it "promotes" what the preview
channel has been testing, and subsequent previews move to `1.0.139-preview-1`.

## How updates flow to users

- **Auto-update follows the stable channel by default.** On launch
  (throttled to once per 24h), `af` resolves the newest release on the
  configured channel and swaps the binary in place. With the default
  `update_channel: "stable"` it asks GitHub's `releases/latest` endpoint,
  which is pinned to the newest non-prerelease release — previews never
  appear there, and there is no release list to page through.
- **Preview tracking is opt-in** via the global config key
  `update_channel: "preview"` (see
  [configuration.md](configuration.md)) — the updater then lists releases
  and picks the version-newest tag including `1.x.y-preview-z`
  prereleases, which are normally the newest. Prereleases are invisible to
  the `releases/latest` API/redirect, so the updater addresses release
  assets by tag.
- **`af upgrade`** resolves through the same configured channel and compares
  the channel's newest release against the running binary before installing,
  so a manual upgrade never downgrades. If the resolved release is *older*
  than the current build — which happens when you flip `update_channel` from
  `preview` back to `stable` while the newest stable is behind the preview
  you're on — the command is a no-op and prints, e.g.,
  `af upgrade would downgrade 1.0.141-preview-2 -> 1.0.140 (stable channel).
  Re-run with --allow-downgrade to proceed.` Passing `--allow-downgrade`
  installs the older release anyway (and prints a `Downgrading X -> Y` notice);
  when you're already on the channel's newest release it reports
  `Already on the latest <channel> release (<version>).`
- **`install.sh` and fresh installs** use the `releases/latest/download/...`
  redirect, which GitHub pins to the newest **stable** — new users start
  (and by default stay) on stable. A specific version (stable or preview)
  can be pinned with `install.sh --version <tag>`.
