# Palette retirement play-test (#3936)

Tested 2026-09-06 using `AF_PLAYTEST_NAME=af-playtest-3936-retirement make
playtest-container`, built from implementation commit `3e1f680b`. The sandbox
used its own daemon, tmux server, throwaway home and mock repository. The harness
provided a bash stand-in; no real agent session was needed for these config flows.
The sandbox was closed after testing.

## Captures and results

- [Appearance](appearance.txt): the selected appearance control shows System and
  exactly `Light · Dark · System`. No shared theme or color editor is present.
- [Edits](edits.txt): Light, Dark and System each saved successfully; CLI reads
  returned `light`, `dark` and `system`. The pane displayed the next-launch notice.
- [Legacy launch](legacy-launch.txt): a raw `theme = 'auto'` config migrated to
  `appearance = 'system'` on TUI launch. `af config list --json` contained
  `appearance=system` and no `theme` or `theme.*` entry.
- [Apply boundary and migration log](apply-boundary.txt): the migration named
  `theme` and `appearance = "system"`; a second read left the migration log count
  at exactly one. Pending listener and token requirements remained on disk.
  The existing listener returned HTTP 200 without a token after launch, and the
  pending port was closed. An explicit config save then moved the listener and
  returned HTTP 401 without a token, proving that explicit apply still works.

## Automated verification

Every new regression test was observed failing before its fix. The failures
included the original launch making an `ApplyTheme` request, old daemons receiving
retired writes, legacy CLI reads failing, unmigrated tables, Auto precedence,
quoted/BOM/CRLF handling, unknown JSON field loss, accidental activation of
TOML-only JSON fields, and a first-run race adopting an unmigrated config.

Passed:

```sh
scripts/testbox.sh test ./config ./daemon ./ui ./commands \
  -run 'Test(PaletteRetirement|Appearance|TerminalBackground|ApplyConfig|ConfigMutations|SetGlobalConfigValue)' -race
go test ./config
go test ./parity
gofmt -l .
go build ./...
go vet ./...
golangci-lint run --timeout=3m --fast
scripts/lint-file-length.sh
scripts/gen-docs.sh
cd web && npm test && npm run build
```

The broad container pass also passed configagent, UI, commands and parity.
All 724 web unit tests passed. Existing P5 appearance tests cover terminal
background detection and dark fallback; a direct browser-mode probe additionally
confirmed both OS modes, explicit-mode precedence and unavailable-detector dark
fallback. No daemon or app tests ran on the host. Both API and CLI references
were regenerated; only the CLI reference changed because the retired launch
operation was a private control RPC.

After integrating newer master changes, all 727 web tests, the bundle rebuild,
reference generation, Go local gates and parity passed again. The only merge
conflict was the generated service-worker cache stamp, resolved by rebuilding.
