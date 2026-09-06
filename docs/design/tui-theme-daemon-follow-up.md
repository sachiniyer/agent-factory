# Daemon palette retirement (#3936)

TUI and web appearance uses fixed generated roles selected independently by
each client. Shared daemon palette configuration is retired.

## Appearance and migration

- Expose only `appearance`: `light`, `dark`, or `system` (default). Do not offer
  Nord/Zenburn, custom color tables or CLI/assistant color-editor routes.
- Migrate legacy `theme = "light"` / `"dark"` to matching appearance. Legacy
  `auto`, `system`, `nord`, `zenburn` and custom `[theme]` tables become
  `system`.
  An existing valid `appearance` always wins. Legacy appearance `auto`
  normalizes
  to `system`; new writes accept only the three supported values. Legacy JSON
  `theme` strings/objects follow the same mapping, as do TOML inline tables and
  dotted table keys such as `theme.accent`.
- Migrate on a normal config read, remove the old key/table, log the old key
  and new appearance value, and persist idempotently. A second read must not
  repeat the migration write or log. This does not require an explicit `af
  config migrate` invocation.
  `LoadConfigReadOnly` and `af config validate` preserve their no-write
  diagnostic
  contract and must not persist migration changes.
- Reject CLI access to `theme` and every `theme.*` key explicitly with a
  retirement error directing users to `appearance`. The full retired slot list
  and value mapping are in [configuration][appearance-migration].
- Never project a retired palette into either client's fixed generated roles.

The TUI reads the preference from its local global config at launch. System
resolves terminal background (OSC 11, then the terminal library's `COLORFGBG`
fallback, then dark); explicit Light/Dark bypass detection. The browser stores
an independent header preference and resolves System from OS appearance.
The CLI edits the TUI preference for its next launch and has no persistent
renderer. A remote config edit changes its named target, not the local TUI's
preference or the browser's independent choice.

Source pointers: `config/appearance.go` validates/normalizes choices;
`app/home_model.go` calls `ui/appearance.go`'s `ApplyAppearance`;
`web/src/ui.ts` offers `THEME_CHOICES`, and `web/src/theme.ts` persists choices
and resolves the browser mode. `parity/inventory.json` records this client-local
capability as `appearance.select`.

## Launch and explicit apply

Remove the launch-only `ApplyTheme` RPC entirely, including its callers and
request/response types. There is no replacement launch config-apply call:
appearance is resolved locally. `GetTheme` is already retired.

A TUI launch must never silently activate pending listener/auth config edits.
Keep explicit apply-on-save for CLI and Config-pane writes, including the
operator-facing failure response for unrelated daemon configuration. Reading
and persisting a legacy appearance migration must not cross that apply boundary.

## Verification requirements

Prove the migration matrix, valid-appearance precedence, removal of all old
slots, migration log contents and second-read idempotence. Prove retired keys
receive explicit CLI errors and old palettes cannot change fixed roles. Prove
TUI launch cannot activate pending listener/auth changes while explicit
apply-on-save remains effective. Keep RPC/schema/parity derivation references
and generated user references consistent with the retired API. Read-only
diagnostics must leave config bytes unchanged, including when they encounter
legacy theme values.

Run daemon and app tests in CI or an approved container harness, never directly
on the host. The `parity` package can be tested independently.

[appearance-migration]:
../configuration.md#appearance-and-legacy-theme-migration
