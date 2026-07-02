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
runs the same preflight, commits the version bump to `main.go`, tags,
builds, and publishes the release marked **latest**.

Releasing the current preview base as-is (e.g. `1.0.138` while
`1.0.138-preview-9` exists) is supported — it "promotes" what the preview
channel has been testing, and subsequent previews move to `1.0.139-preview-1`.

## How updates flow to users

- **Auto-update follows the preview channel.** On launch (throttled to once
  per 24h), `af` lists GitHub releases, picks the newest tag across both
  channels — normally the newest preview; the stable itself right after a
  stable release — and swaps the binary in place. Prereleases are invisible
  to the `releases/latest` API/redirect, so the updater addresses release
  assets by tag.
- **`af upgrade`** resolves the same way, so a manual upgrade never
  downgrades a preview build back to an older stable.
- **`install.sh` and fresh installs** use the `releases/latest/download/...`
  redirect, which GitHub pins to the newest **stable** — new users start on
  stable, then auto-update onto the preview channel. A specific version
  (stable or preview) can be pinned with `install.sh --version <tag>`.
