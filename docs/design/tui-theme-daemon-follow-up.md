# P5 daemon theme boundary

P5 consumes fixed generated TUI roles. It does not change `daemon/theme.go` or
`ApplyTheme`; P2 slice C (#3929) owns that boundary while it lands.

The remaining shared configuration work needs a single appearance mode with
values `light`, `dark`, and `system`, matching the web choice. A legacy `auto`
value maps to `system`; retired Nord/Zenburn/custom palettes must not be projected
into token overrides. System resolves at each client: terminal background in the
TUI (dark fallback), OS appearance in the browser.

After #3929 lands, remove the daemon's legacy palette parsing/projection and
colour-editor schema routes together. Audit CLI configuration and assistant
routes, preserve unrelated listener/auth changes behind their existing explicit
apply boundary, and decide whether the launch-only ApplyTheme operation can be
retired. P5 must not repurpose that operation to apply unrelated config edits.
Tests should prove old colour tables cannot change either client's fixed roles,
legacy auto migrates to system, and theme-only launch handling does not activate
pending authentication/listener edits. Run daemon tests only in CI or the approved
container harness.
