// The CONFIG view of the web client: the browser analogue of the TUI's config
// editor overlay (ui/config_pane.go), and the direct counterpart to the
// conversational config agent (#1928). All three render from ONE description of
// configuration — the config manifest (config/manifest.go), delivered here by
// GetConfig — so none of them can fall behind config_types.go or each other.
//
// This file therefore contains NO list of config keys, no per-key type switch,
// and no copy of the defaults or the validation rules. It renders whatever the
// manifest says exists, in the tiers the manifest ranks, and it sends every edit
// to SetConfigValue, which the daemon hands to the same validated, file-locked,
// atomic writer `af config set` uses. A key added to config_types.go appears
// here with no edit to this file — that is the point, and
// config.test.ts pins it.
//
// Two things it must never do:
//
//   - Validate locally. A second copy of the rules is how a UI comes to accept a
//     value the loader rejects at startup, which the user then meets as a
//     failure to start instead of a red line under a field. The daemon's error
//     is shown verbatim.
//   - Imply an edit is live. config.toml is read at STARTUP, so a saved value
//     reaches af and the daemon on their next start. The restart notice comes
//     from the daemon on every successful write and is shown next to the echo,
//     at the moment of the edit.
//
// Patched in place like the rest of the shell and CSP-safe (createElement +
// addEventListener via the shared h() helper, no innerHTML with markup).

import {
  ACCOUNT_INPUT_ATTR,
  type AccountActions,
  type AccountsState,
  emptyAccountsState,
  renderAccountsSection,
} from "./accounts.js";
import { h } from "./dom.js";
import type { ConfigEntry, ConfigSetResponse } from "./types.js";
import { rebuildKeepingScroll } from "./scrollkeep.js";

/** The config list is ONE global manifest, so every rebuild shows the same list and
 *  the reader's place is always worth keeping — there is no project or filter here to
 *  change what the list contains. */
const CONFIG_LIST_TOKEN = "config";

/** What the config view can ask the shell to do. Saving is the shell's job (it
 *  owns the token and the refresh), so the pane reports intent and renders the
 *  outcome it is handed back. */
export interface ConfigActions {
  save: (key: string, value: string) => void;
  /** Opens the conversational config assistant (#2467) — the web analogue of the
   *  TUI's config-agent takeover. The shell owns the token and the modal host, so
   *  the pane only reports the intent. */
  openAssistant: () => void;
  /** The Accounts section's two verbs (#3385). They are NOT config writes and do
   *  not go through `save`: an account is a credential directory on the daemon
   *  host, not a settable key, and routing them through the config writer would
   *  be the category error #3385 asks this surface to avoid. */
  accounts: AccountActions;
}

/** The outcome of the last save, as the shell learned it from the daemon. */
export interface ConfigStatus {
  key: string;
  /** The echo — the canonical value the daemon actually wrote, which may differ
   *  from what was typed. Empty when `error` is set. */
  value: string;
  /** The daemon's save outcome and warnings, shown verbatim. */
  notice: string;
  /** The validator's message, verbatim, when the value was refused. */
  error: string;
}

/** The tier headings, in the manifest's own order. Derived from the entries
 *  rather than hardcoded so a new tier needs no edit here. */
function tiersInOrder(entries: ConfigEntry[]): { tier: number; name: string }[] {
  const seen = new Map<number, string>();
  for (const e of entries) {
    if (!seen.has(e.tier)) {
      seen.set(e.tier, e.tier_name);
    }
  }
  return [...seen.entries()].sort((a, b) => a[0] - b[0]).map(([tier, name]) => ({ tier, name }));
}

/** The advanced tier's rank. Tier 3 is "correct by default and rarely touched",
 *  so it folds — the five keys that matter must not be buried under twenty. */
const TIER_ADVANCED = 3;

/** The kind of control a key gets. */
export type ControlKind = "checkbox" | "select" | "text";

/**
 * Chooses the control for a key FROM THE MANIFEST'S OWN DESCRIPTION of it —
 * never from the key's name, and never from a local table of known keys.
 *
 * This is the function that makes the form survive a config key it has never
 * heard of: an unfamiliar type still falls through to a text field rather than
 * being dropped from the form or throwing. It is exported so config.test.ts
 * locks the REAL decision rather than a copy of it — a copy would drift from
 * renderControl, which is the very failure this whole design is avoiding.
 *
 *  - `bool` → checkbox.
 *  - enumerated → picker. Excluded for a table, where the enum constrains entry
 *    NAMES rather than the value; offering it as a value picker would be a small
 *    lie about what the key takes.
 *  - anything else → text.
 *
 * A pre-#3345 daemon's retired `editable: false` wire member is deliberately
 * absent from ConfigEntry and ignored here. Recreating a read-only fallback
 * would violate the zero-read-only surface; during version skew the real field
 * instead shows that old daemon's explicit save rejection verbatim.
 */
export function controlKind(e: ConfigEntry): ControlKind {
  if (e.type === "bool") {
    return "checkbox";
  }
  if (e.enum && e.enum.length > 0 && e.type !== "table") {
    return "select";
  }
  return "text";
}

/**
 * Whether a field's current text is worth writing: only when it differs from what
 * the file already holds.
 *
 * A no-op write is not harmless. It still round-trips to the daemon, still prints
 * `set key = value`, and still raises the restart notice — so it reads as though
 * the user changed something and now owes a restart. Exported so config.test.ts
 * locks the REAL rule both the Save button and the Enter key gate on, rather than
 * a copy of it.
 */
export function canCommit(shown: string, current: string): boolean {
  return shown !== current;
}

/**
 * Whether a freshly delivered status should CLOSE the field that is open.
 *
 * A save closes the field it came from: the value is committed, and leaving it open
 * would invite a second write of the same thing. The subtlety is `statusIsNew`. A
 * success status is a LEVEL that stays on the store, not an EDGE that fires once —
 * `applyConfigValueNow` sets `configStatus` and then calls `refreshConfig()`, whose
 * commit writes only `config`/`configPath`. The refetch therefore re-enters update()
 * with fresh entries and the SAME status object, and a predicate that ignores the
 * transition closes whatever edit is open when it lands.
 *
 * That window is not hypothetical: it is precisely the window this view's focus
 * restoration exists to make usable, so the edit sitting in it is the user typing
 * again in the field they just saved. Closing it clears `editing` and `draft`, and
 * the rebuild then paints the re-read value over text that was on screen a frame
 * earlier — a silent loss, with no error and nothing to undo.
 *
 * The failure direction is deliberate. Requiring the edge can only ever leave an
 * edit OPEN that a re-delivery would have closed, and an edit that outlived its own
 * save is by definition typing the user did afterwards — the thing to keep. A field
 * left open costs a redundant Save at worst; a field closed early costs the user
 * their input. Exported so config.test.ts locks the rule rather than a copy of it.
 */
export function shouldCloseSavedField(
  status: ConfigStatus | null,
  editing: string | null,
  statusIsNew: boolean,
): boolean {
  return statusIsNew && status !== null && !status.error && status.key === editing;
}

/**
 * The one line the form shows under a saved field: the daemon's own account of
 * when the edit takes effect, plus — when the edit moved a listener — where the
 * daemon is accepting now (#3722).
 *
 * Both halves come from the daemon and neither is computed here. The address in
 * particular MUST NOT be inferred from the value that was sent: a rebind can
 * fail, in which case the daemon is still serving on the previous address and
 * says so, and a form that echoed what it typed would name an address nothing
 * answers precisely when the user most needs the real one.
 *
 * Warnings come from TWO channels on the SetConfigValue response, both owned by
 * the daemon:
 *
 *   - Channel 1, `resp.result.warnings` (config.SetResult.Warnings): per-write
 *     notes authored by the config writer itself — e.g. a `default_accounts.<agent>`
 *     write whose named account is not registered, or the tokenless-network-listener
 *     exposure notice. Always present on the wire because the daemon sets
 *     `resp.Result = result` before returning; `omitempty` drops the slice when
 *     it is empty, so the browser sees `undefined`.
 *   - Channel 2, `resp.warnings` (SetConfigValueResponse.Warnings): apply-time,
 *     authoritative warnings the daemon maintains about the running config — a
 *     failed live apply, a rebind failure, the post-apply exposure state.
 *
 * The two overlap only when both the writer and the applier describe the same
 * hazard in different words (the tokenless-exposure case). For a write that
 * succeeds and has no apply-side counterpart — exactly the `default_accounts`
 * case, which is `EffectAppliedLive` and produces no apply warning — channel 1
 * carries the only warning and channel 2 is silent. A surface that reads only
 * channel 2 (as this form did before this fix) loses the per-write advisory in
 * exactly that case, which is the one place that tells an operator, at save
 * time, that the account they defaulted to is unregistered and how to fix it.
 *
 * Channel 2 is NOT the complete set on a successful apply, however. The server's
 * completeConfigSaveWarnings keeps it authoritative on a failed/unconfirmed
 * apply (merging both channels into it) but on a successful apply it returns
 * ONLY the apply warnings, dropping the writer's per-write advisories. A
 * pending listener rebind retried on an UNRELATED save (e.g. a
 * `default_accounts` write) produces a rebind failure on channel 2 while
 * channel 1 carries the unregistered-account advisory the write produced, and
 * a channel-2-only read would lose it. The form therefore MERGES both channels
 * rather than choosing one, suppressing two kinds of duplication after channel
 * 2: a write warning already copied into channel 2 verbatim by the server's
 * failed/unconfirmed merge, and the tokenless-exposure forecast (exposureWarning)
 * for a listener channel 2 already carries the authoritative notice
 * (ListenerExposureNotice/PreviewListenerExposureNotice) for — matched by listener
 * key AND address, so a notice for network.listen_addr at one address does not
 * suppress a forecast for network.listen_addr at a different address (a failed
 * rebind can leave the serving address and the configured address different), nor
 * a notice for network.listen_addr suppress a forecast for
 * network.preview_listen_addr. Other per-write advisories (the
 * unregistered-account warning, the project-scoped branch_prefix notice) have no
 * apply-side counterpart and are always preserved.
 *
 * The restart notice is normally shown only for a key that requires a restart,
 * since "Applied" under every field is noise. A warning overrides that gate:
 * it is actionable for an ordinarily-live key, and both the warning and the
 * outcome sentence must reach the operator. The address is also independent of
 * the gate: a listener that moved is worth saying whether or not a restart is
 * owed, and for this form it is the address the browser must be re-pointed at.
 * Exported so config.test.ts locks the real rule rather than a copy.
 */
// isListenerExposureWarning identifies the tokenless-network-listener exposure
// notice — the one hazard both warning channels describe. The writer's
// exposureWarning (config/configset.go, channel 1) and the applier's
// ListenerExposureNotice / PreviewListenerExposureNotice (config/authposture.go,
// channel 2) all phrase it as "<key> ... is reachable from the network ...".
// The rebind-failure error ("apply network.listen_addr ...") and the
// unregistered-account advisory ("no <agent> account named ...") do not.
function isListenerExposureWarning(w: string): boolean {
  return w.includes("is reachable from the network");
}

// listenerExposureIdentity returns the listener key AND the quoted address an
// exposure warning describes — e.g. `network.listen_addr 0.0.0.0:9443` — so a
// forecast is deduplicated only against an authoritative notice for the SAME
// listener at the SAME address. Both channels format the exposure identically:
// the key, then a quoted address, then "is reachable from the network ...", so
// the identity is the key plus the address between the first quotes. Matching by
// key alone is not enough: when a listener rebind has failed and the serving
// address differs from the configured one, channel 2 (ListenerExposureNotice)
// names the serving bound address while channel 1 (exposureWarning) forecasts the
// configured address that will be exposed after restart
// (TestApplyConfigExposureNoticePerKeyOrderNamesServingAddress pins the serving
// vs requested split). A notice for the serving address is not an authoritative
// copy of a forecast for a different address the listener has not yet bound, so
// the address is part of the identity. Two different listeners can each carry an
// exposure notice (a successful control-plane rebind on channel 2 while a
// preview rebind failed on channel 1, or the inverse), and a notice for one is
// not an authoritative copy of a forecast for the other. Returns "" for a
// warning that is not an exposure warning; callers gate on
// isListenerExposureWarning first.
//
// The identity is exact-address. When the configured address is ephemeral (port
// :0), a successful rebind resolves it to a concrete kernel-assigned port that
// channel 2 names, so the exact-address identity would not match and both
// versions of the same exposure would render. listenerExposureDedup handles that
// relaxation; see its comment for the constraint that keeps the failed-rebind
// case (two different concrete addresses) distinct.
//
// The address is quoted with Go's %q, which escapes a literal " as \" (and \ as
// \\). An IPv6 zone identifier can contain a " (e.g. [::%foo"bar]:0), and a
// non-greedy [^"]+ would stop at the escaped quote, leaving a truncated identity
// that splitListenHostPort cannot parse and preventing dedup. The regex
// (?:[^"\\]|\\.)* matches either a non-quote/non-backslash character or a
// backslash escape (\" or \\), so the closing quote is the real terminator. The
// captured group is then unescaped (\" → ", \\ → \) to recover the literal
// address Go's net.Addr().String() reports.
function listenerExposureIdentity(w: string): string {
  const m = w.match(/^(\S+) "((?:[^"\\]|\\.)*)"/);
  if (!m) return "";
  const addr = m[2].replace(/\\(.)/g, "$1");
  return `${m[1]} ${addr}`;
}

// listenerExposureDedup returns true when a write-side exposure forecast
// (channel 1, exposureWarning) is the same hazard an apply-side notice (channel 2,
// ListenerExposureNotice/PreviewListenerExposureNotice) already covers
// authoritatively. It compares listenerExposureIdentity values, with three
// relaxations for the same-listener case:
//
//   1. Wildcard host normalization: a configured 0.0.0.0 (IPv4 wildcard) and a
//      serving [::] (IPv6 wildcard) are the same "all interfaces" bind on a
//      dual-stack host — net.Listen("tcp", "0.0.0.0:0") can report its bound
//      address as [::]:<port>, so channel 1's 0.0.0.0 and channel 2's [::] are
//      the same listener. normalizeListenHost canonicalizes IPv6 hosts to their
//      RFC 5952 form (so 0:0:0:0:0:0:0:0 and :: compare equal) and maps the
//      empty host (":8443") to the same "0.0.0.0"; a concrete host (loopback,
//      LAN IP) is returned as-is. The cross-family wildcard equivalence (:: ->
//      0.0.0.0) is gated on bindSucceeded: on a host with IPV6_V6ONLY the IPv6
//      and IPv4 wildcards expose different address families, so only a
//      successful rebind (the daemon bound the configured address and
//      net.Addr().String() reported the other family) confirms they are the
//      same bind. Without that confirmation (a require_token flip, or a
//      failed/deferred rebind) the configured [::] and the serving 0.0.0.0 may
//      be different sockets, and the forecast and the notice must both surface.
//   2. Resolved port (bindSucceeded only): a successful rebind resolves the
//      configured port to the kernel-assigned serving port — :0 (the ephemeral
//      wildcard) to a concrete port, and a service name (e.g. "http-alt",
//      "mysql") to its numeric port via net.LookupPort (a set far larger than
//      the WELL_KNOWN_TCP_SERVICES fallback in normalizeListenPort). The
//      browser cannot reproduce either resolution, so when the rebind SUCCEEDED
//      (bindSucceeded) a :0 or service-name forecast matches any concrete
//      numeric serving port for the same listener and (normalized) host. Gated
//      on bindSucceeded: a failed rebind leaves the serving address as the OLD
//      bound address, so a :0 or service-name forecast names a future bind
//      distinct from the current serving notice, and both must surface.
//   3. Hostname bind (bindSucceeded only): a configured non-literal host (a
//      hostname like "myhost" the validator accepts) resolves to a literal IP at
//      bind time, so channel 1 names the configured hostname while channel 2 names
//      the kernel-resolved IP. The browser cannot reproduce DNS, so a textual host
//      comparison cannot match them; instead, when the listener rebind SUCCEEDED
//      (the saved key is a listener key and apply_outcome is "applied"), the
//      configured host is the bind the serving address resolved from, and the
//      dedup matches by listener key and port alone. The browser gates this on
//      the daemon's success signal because it cannot resolve the hostname itself:
//      isListenHostLiteral distinguishes a numeric IP (the serving address form)
//      from a hostname (the configured form), and bindSucceeded is the apply's
//      per-key outcome, which is "applied" only when the rebind landed and
//      "deferred" when it failed (config/effect.go saveRules). The failed-rebind
//      case below is preserved: a failed rebind is "deferred", not "applied", so
//      bindSucceeded is false and the strict host comparison still applies.
//
// The failed-rebind case where both addresses are concrete and differ is preserved:
// two different concrete ports for the same host do not match, and two different
// hosts do not match regardless of the port (TestApplyConfigExposureNoticePerKey
// OrderNamesServingAddress pins the serving-vs-configured split for a failed
// rebind). Zero-padded zero ports ("00", "000") and an omitted port ("0.0.0.0:")
// are normalized to "0" (net.LookupPort resolves them to 0, the ephemeral wildcard).
// Service-name ports the table knows ("http", "https", …) are resolved to their
// numeric ports (normalizeListenPort → WELL_KNOWN_TCP_SERVICES) only on a
// successful rebind (bindSucceeded): Go's net.LookupPort augments its built-in
// mappings from the daemon host's /etc/services, so the browser-side table is not
// authoritative on a host that maps a name to a different port, and on a failed
// rebind the configured service name is a future state the kernel has not
// resolved. On a successful rebind the table is a convenience for the common case
// (http→80), and the resolved-port relaxation above matches a service-name
// forecast to any concrete numeric serving port without the table; on a failed
// rebind (or a non-listener save) the service name is returned unchanged so a
// host-specific mapping never suppresses a warning for a different restart port.
// Service names the table does NOT carry (http-alt, mysql, … — net.LookupPort
// reads /etc/services, which is larger than the browser-side fallback) are handled
// by the same resolved-port relaxation on a successful rebind.
function listenerExposureDedup(writeId: string, applyId: string, bindSucceeded: boolean, rebindAttempted: boolean): boolean {
  if (writeId === applyId) {
    return true;
  }
  // The identity is "<key> <address>"; the key (network.listen_addr or
  // network.preview_listen_addr) has no spaces, so the address is the rest.
  const wSpace = writeId.indexOf(" ");
  const aSpace = applyId.indexOf(" ");
  if (wSpace < 0 || aSpace < 0) {
    return false;
  }
  if (writeId.slice(0, wSpace) !== applyId.slice(0, aSpace)) {
    return false;
  }
  const wAddr = splitListenHostPort(writeId.slice(wSpace + 1));
  const aAddr = splitListenHostPort(applyId.slice(aSpace + 1));
  if (!wAddr || !aAddr) {
    return false;
  }
  if (normalizeListenHost(wAddr.host, bindSucceeded) !== normalizeListenHost(aAddr.host, bindSucceeded)) {
    // A successful listener rebind resolves a non-literal configured host (a
    // hostname the validator accepts, e.g. the machine's hostname) to the
    // literal serving address net.Addr().String() reports. The browser cannot
    // reproduce that resolution, so rather than comparing the hostname against
    // the IP textually, treat them as the same bind when the rebind succeeded
    // (bindSucceeded, from the per-key apply_outcome) and the configured host is
    // a hostname (not a numeric IP — isListenHostLiteral) while the serving host
    // is a literal IP. The failed-rebind case (a prior rebind left the listener
    // serving a different address) is preserved: a failed rebind is "deferred",
    // not "applied", so bindSucceeded is false and the strict host comparison
    // keeps two different concrete addresses distinct.
    //
    // The hostname relaxation additionally requires rebindAttempted — positive
    // evidence that a rebind was actually attempted for this listener on this
    // save (the saved key is the listener key). A non-listener save (e.g.
    // network.require_token) does not rebind the listener when the configured
    // address is unchanged: the daemon only calls bindWebLocked when the
    // configured address text differs from webConfigAddr. apply_outcome "applied"
    // for a non-listener key and the absence of a rebind failure warning do not
    // confirm a rebind — the listener may already be at the configured address,
    // in which case no bind happened and the serving IP (channel 2) is the
    // stale DNS resolution from the previous bind. If DNS changed since that
    // bind, channel 1's hostname forecast names the hostname that will resolve
    // to a different IP after restart, so suppressing it would hide a distinct
    // future-state warning. Gating on rebindAttempted (the saved key is the
    // listener key) ensures the relaxation fires only when the daemon actually
    // rebound and resolved the hostname at this save's bind time.
    if (!(bindSucceeded && rebindAttempted && !isListenHostLiteral(wAddr.host) && isListenHostLiteral(aAddr.host))) {
      return false;
    }
  }
  const wPort = normalizeListenPort(wAddr.port, bindSucceeded);
  const aPort = normalizeListenPort(aAddr.port, bindSucceeded);
  if (wPort === aPort) {
    return true;
  }
  // A successful rebind resolves the configured port to the kernel-assigned
  // serving port. Two configured forms the browser cannot reproduce on its own:
  //   - :0 (the ephemeral wildcard) → a concrete kernel-assigned port.
  //   - a service name (e.g. "http-alt", "mysql") → its numeric port, via
  //     net.LookupPort reading /etc/services — a set far larger than the
  //     WELL_KNOWN_TCP_SERVICES fallback in normalizeListenPort, so the table
  //     cannot cover every name the validator accepts (config/listen_addr.go).
  // When the rebind SUCCEEDED (bindSucceeded), the configured port IS the
  // serving port — the kernel resolved it — so a :0 or service-name forecast
  // matches any concrete numeric serving port for the same listener and
  // (normalized) host. This is gated on bindSucceeded: a failed rebind leaves
  // the serving address as the OLD bound address, so a :0 or service-name
  // forecast names a future bind distinct from the current serving notice, and
  // both must surface (TestApplyConfigExposureNoticePerKeyOrderNamesServing
  // Address pins the serving-vs-configured split for a failed rebind). The
  // apply channel never carries :0 (it names the serving bound address), so
  // only the forecast direction needs the relaxation; the inverse — a concrete
  // forecast against a :0 notice — does not arise.
  //
  // isDecimalPort (not Number()) recognizes the same decimal port grammar Go's
  // net.LookupPort treats as numeric: a hex service name like "0x50" that
  // /etc/services maps to a different port must not be treated as the decimal
  // port Number("0x50") would coerce it to. On a successful rebind the
  // configured service name IS the serving port the kernel resolved it to, so
  // the relaxation matches a non-decimal forecast (a service name like "0x50"
  // or "http-alt") to any concrete numeric serving port; on a failed rebind the
  // service name is a future state the kernel has not resolved, and the strict
  // port comparison (isDecimalPort only) keeps the distinct serving port
  // surfaced (normalizeListenPort returns the name unchanged when
  // bindSucceeded is false).
  if (bindSucceeded && aPort !== "0" && isDecimalPort(aPort)) {
    if (wPort === "0" || !isDecimalPort(wPort)) {
      return true;
    }
  }
  return false;
}

// isListenHostLiteral reports whether a host part (already split by
// splitListenHostPort, so brackets are stripped for IPv6) is a numeric IP address
// rather than a hostname. A configured address can be a hostname the validator
// accepts (config/listen_addr.go) and net.Listen resolves to a literal IP at bind
// time; the serving address (channel 2, from net.Addr().String()) is always a
// literal. listenerExposureDedup uses this to distinguish the two when a
// successful rebind resolved the hostname, so a hostname forecast and the literal
// serving notice are the same bind rather than two different exposures.
//   - IPv4: a dotted-quad of canonical decimal octets (no leading zero except
//     "0" itself), each at most 255, matching the form the validator and
//     net.Listen accept (and net.Addr().String() reports verbatim). A
//     dotted-quad whose octets are all 1-3 digits but are not all ≤255
//     (e.g. 256.1.1.1) is not a valid IPv4 address — Go's net.Listen treats
//     it as a hostname and resolves it via DNS — so it must not be classified
//     as a literal; the hostname relaxation then matches it to the serving IP
//     on a successful rebind instead of treating it as a distinct literal that
//     the strict host comparison can never equal. A dotted-quad with a leading
//     zero (e.g. 001.2.3.4) is likewise not a valid IPv4 address — Go's
//     net.Listen rejects leading-zero octets and resolves the name as a
//     hostname — so it too must be classified as a non-literal for the
//     hostname relaxation to match it to the serving IP on a rebind.
//   - IPv6: any host containing a colon — splitListenHostPort already stripped
//     the brackets, so "::", "2001:db8::1" etc. are bare IPv6.
//   - Everything else is a hostname (a name the resolver will look up), including
//     "localhost" and "myhost"; both are non-literal and are left for the hostname
//     relaxation to treat as the bind's configured name.
function isListenHostLiteral(host: string): boolean {
  if (host.includes(":")) {
    return true;
  }
  const octets = host.split(".");
  if (octets.length !== 4) return false;
  return octets.every((o) => /^(0|[1-9]\d{0,2})$/.test(o) && Number(o) <= 255);
}

// splitListenHostPort splits a "<host>:<port>" or "[<host>]:<port>" address into
// its host and port, mirroring net.SplitHostPort. The identity's address comes
// from the daemon's %q formatting of a net.Addr, so an IPv6 wildcard is
// "[::]:<port>". Returns null for an address that does not parse.
function splitListenHostPort(addr: string): { host: string; port: string } | null {
  // IPv6: [host]:port — the bracketed host is the part inside [], the port
  // follows the closing bracket and a colon.
  if (addr.startsWith("[")) {
    const close = addr.indexOf("]");
    if (close < 0) return null;
    const after = addr.slice(close + 1);
    if (!after.startsWith(":")) return null;
    return { host: addr.slice(1, close), port: after.slice(1) };
  }
  // IPv4 or empty host: the last colon separates host and port. An empty host
  // (":8443") is valid (binds all interfaces) and yields host "".
  const colon = addr.lastIndexOf(":");
  if (colon < 0) return null;
  return { host: addr.slice(0, colon), port: addr.slice(colon + 1) };
}

// normalizeListenHost canonicalizes "all interfaces" wildcard hosts so a
// configured 0.0.0.0 (IPv4) and a serving [::] (IPv6, which net.Listen("tcp",
// "0.0.0.0:0") reports on dual-stack hosts) compare equal. An empty host (e.g.
// ":8443") also binds every interface. A concrete host (loopback, LAN IP) is
// returned as-is.
//
// IPv6 hosts are parsed and re-emitted in their RFC 5952 canonical form
// (canonicalizeIPv6Host), so a non-canonical configured spelling the validator
// accepts — e.g. "0:0:0:0:0:0:0:0" — compares equal to the compressed "::"
// form net.Addr().String() reports for the serving address. IPv4-mapped
// (::ffff:0.0.0.0) is collapsed to the bare IPv4 address by
// canonicalizeIPv6Host (matching net.IP.String via IP.To4), so a configured
// [::ffff:0.0.0.0]:0 — the IPv4-mapped wildcard — compares equal to a
// serving [::]:0 on a dual-stack bind. IPv4-compatible (::1.2.3.4) is NOT
// collapsed — Go's net.ParseIP("::1.2.3.4").String() is "::102:304" (not
// "1.2.3.4") and To4() is nil, so it is a distinct IPv6 address. Matching only
// the already-compressed "::" left the uncompressed wildcard and its canonical form
// as different literals, so both versions of the same exposure rendered. The
// IPv6 unspecified address ("::") is the all-interfaces wildcard on a
// dual-stack host and maps to the IPv4 "0.0.0.0" wildcard, matching the
// long-standing dual-stack equivalence.
//
// The cross-family :: -> 0.0.0.0 mapping is gated on bindSucceeded: on a host
// with IPV6_V6ONLY, [::]:8443 and 0.0.0.0:8443 expose different address
// families, so only a successful rebind (the daemon bound the configured
// address and net.Addr().String() reported the other family) confirms they are
// the same bind. Without that confirmation (a require_token flip, or a
// failed/deferred rebind) the configured [::] and the serving 0.0.0.0 may be
// different sockets, so both the forecast and the notice must surface. The
// within-family canonicalization (RFC 5952 form, IPv4-mapped -> IPv4) is not
// gated — it is a same-address comparison, not a cross-family equivalence.
function normalizeListenHost(host: string, bindSucceeded: boolean): string {
  if (host === "") {
    return "0.0.0.0";
  }
  if (host.includes(":")) {
    const v6 = canonicalizeIPv6Host(host);
    // The IPv6 unspecified address ("::") is the all-interfaces wildcard on a
    // dual-stack host. Map it to the IPv4 "0.0.0.0" wildcard only when
    // bindSucceeded: a successful rebind confirms the configured and serving
    // addresses are the same bind (the daemon bound the configured address and
    // net.Addr().String() reported the other family). On a host with
    // IPV6_V6ONLY the two families expose different interfaces, so without that
    // confirmation (a require_token flip, or a failed/deferred rebind) the
    // configured [::] and the serving 0.0.0.0 may be different sockets.
    if (v6 === "::") {
      return bindSucceeded ? "0.0.0.0" : "::";
    }
    return v6;
  }
  return host;
}

// canonicalizeIPv6Host parses a bare (bracket-stripped) IPv6 host into its 8
// 16-bit groups and re-emits the RFC 5952 canonical form: lowercase hex, a
// single "::" compressing the longest run of consecutive zero groups (length
// >= 2), and no leading zeros per group. net.Addr().String() already emits
// this form for the serving address (channel 2), so canonicalizing a
// non-canonical configured spelling (channel 1, e.g. "0:0:0:0:0:0:0:0")
// lets the two compare equal. IPv4-mapped (::ffff:0.0.0.0) is emitted as the
// bare IPv4 dotted-quad, matching net.IP.String()/net.Addr().String() which
// emits the IPv4 form via IP.To4; a dual-stack net.Listen("tcp",
// "[::ffff:0.0.0.0]:0") reports "[::]:<port>", and ::ffff:0.0.0.0
// canonicalizes to 0.0.0.0 (the IPv4 wildcard) which normalizeListenHost
// already maps to the same value as ::. IPv4-compatible (::1.2.3.4, the first
// 96 bits zero) is NOT collapsed: Go's net.ParseIP("::1.2.3.4").String() is
// "::102:304" (not "1.2.3.4") and To4() is nil, so it is a distinct IPv6
// address, not IPv4 — a failed-rebind forecast for [::1.2.3.4]:8443 against a
// serving 1.2.3.4:8443 must not be deduped as the same address.
// A zone identifier (%zone, RFC 4007) is stripped before parsing so
// parseIPv6Groups does not reject it, but is PRESERVED for scoped non-wildcard
// addresses: Go's net.TCPAddr.String() retains the zone for scoped
// non-wildcard addresses (e.g. [fe80::1%eth0]:8443), so a failed rebind that
// leaves the daemon serving [fe80::1%eth0]:8443 while config carries
// [fe80::1%eth1]:8443 must not compare equal — stripping both zones would
// suppress the future-state forecast. The all-zeros wildcard (::) drops the
// zone because Go's net.Addr().String() drops it from the bound address, so
// [0:0:0:0:0:0:0:0%lo] canonicalizes to :: (the zone is irrelevant for a
// dual-stack wildcard).
// Returns the input unchanged when it is not a parseable bare IPv6 address
// (a hostname or an IPv4 host).
function canonicalizeIPv6Host(host: string): string {
  let zone = "";
  const pct = host.indexOf("%");
  if (pct >= 0) {
    zone = host.slice(pct);
    host = host.slice(0, pct);
  }
  const groups = parseIPv6Groups(host);
  if (groups === null) {
    return host + zone;
  }
  // IPv4-mapped (::ffff:a.b.c.d): the first 80 bits are zero and bytes 10-11
  // are 0xffff. Go's net.IP.To4() collapses this to a 4-byte IPv4 address, and
  // net.IP.String() emits the dotted-quad, so canonicalizing to the IPv4 form
  // matches the serving address net.Addr().String() reports for a bind the
  // validator accepts. IPv4-compatible (::a.b.c.d, the first 96 bits zero) is
  // NOT collapsed: Go's net.ParseIP("::1.2.3.4").String() is "::102:304" (not
  // "1.2.3.4") and To4() is nil, so it is a distinct IPv6 address, not IPv4 —
  // a failed-rebind forecast for [::1.2.3.4]:8443 against a serving 1.2.3.4:8443
  // must not be deduped as the same address. The zone is not relevant for an
  // IPv4-mapped address (net.Addr().String() reports the bare IPv4 form).
  if (groups[0] === 0 && groups[1] === 0 && groups[2] === 0 && groups[3] === 0 && groups[4] === 0 &&
      groups[5] === 0xffff) {
    const hi = groups[6];
    const lo = groups[7];
    return `${(hi >> 8) & 0xff}.${hi & 0xff}.${(lo >> 8) & 0xff}.${lo & 0xff}`;
  }
  // Find the longest run of consecutive zero groups (length >= 2) to compress.
  let bestStart = -1;
  let bestLen = 0;
  let curStart = -1;
  let curLen = 0;
  for (let i = 0; i < 8; i++) {
    if (groups[i] === 0) {
      if (curStart < 0) curStart = i;
      curLen++;
      if (curLen > bestLen) {
        bestLen = curLen;
        bestStart = curStart;
      }
    } else {
      curStart = -1;
      curLen = 0;
    }
  }
  if (bestLen < 2) bestStart = -1;
  const parts: string[] = [];
  let i = 0;
  while (i < 8) {
    if (bestStart >= 0 && i === bestStart) {
      // The "::" is the join of two empty entries; push an empty before and
      // after the run so join emits "::", "::1", "1::", or "a::b" correctly.
      if (bestStart === 0) parts.push("");
      parts.push("");
      i += bestLen;
      if (i === 8) parts.push("");
    } else {
      parts.push(groups[i].toString(16));
      i++;
    }
  }
  const canonical = parts.join(":");
  // Preserve the zone for scoped non-wildcard addresses. Go's
  // net.TCPAddr.String() retains the zone for scoped non-wildcard addresses
  // (e.g. fe80::1%eth0), so two different scoped interfaces (fe80::1%eth0 vs
  // fe80::1%eth1) must NOT compare equal — a failed rebind can leave the daemon
  // serving one zone while config carries another, and the future-state
  // forecast for the distinct zone must surface. The all-zeros wildcard (::)
  // drops the zone: Go's net.Addr().String() drops it from the bound address
  // (the zone is irrelevant for a dual-stack wildcard), so the canonical form
  // is zone-less.
  if (canonical === "::") {
    return canonical;
  }
  return canonical + zone;
}

// parseIPv6Groups parses a bare IPv6 host (brackets already stripped) into its
// 8 16-bit groups, handling a single "::" zero-run compression and an IPv4-
// embedded tail (the last colon-separated group is a dotted quad, e.g.
// "::ffff:0.0.0.0"). The IPv4 tail occupies the final two 16-bit groups.
// Returns null for anything that is not a parseable IPv6 address (a hostname or
// an IPv4 host without a colon).
//
// A zone identifier (%zone, RFC 4007 — e.g. "0:0:0:0:0:0:0:0%lo") is stripped
// before parsing so the group parser does not reject it. canonicalizeIPv6Host
// extracts and preserves the zone for scoped non-wildcard addresses (Go's
// net.TCPAddr.String() retains it, e.g. [fe80::1%eth0]:8443), dropping it only
// for the all-zeros wildcard (::, where net.Addr().String() drops it from the
// bound address).
function parseIPv6Groups(host: string): number[] | null {
  const pct = host.indexOf("%");
  if (pct >= 0) {
    host = host.slice(0, pct);
  }
  const dbl = host.indexOf("::");
  if (dbl >= 0 && host.indexOf("::", dbl + 2) >= 0) return null;
  let head: string[];
  let tail: string[];
  let ipv4Groups: [number, number] | null = null;
  if (dbl < 0) {
    const parts = host.split(":");
    // An IPv4-embedded tail (the last segment is a dotted quad) occupies two
    // 16-bit groups, so 7 hex groups + 1 IPv4 part makes 8.
    if (parts.length > 0 && parts[parts.length - 1].includes(".")) {
      ipv4Groups = parseIPv4Embedded(parts[parts.length - 1]);
      if (ipv4Groups === null) return null;
      if (parts.length !== 7) return null;
      head = parts.slice(0, 6);
      tail = [];
    } else {
      if (parts.length !== 8) return null;
      head = parts;
      tail = [];
    }
  } else {
    const h = host.slice(0, dbl);
    const t = host.slice(dbl + 2);
    head = h === "" ? [] : h.split(":");
    tail = t === "" ? [] : t.split(":");
    if (tail.length > 0 && tail[tail.length - 1].includes(".")) {
      ipv4Groups = parseIPv4Embedded(tail[tail.length - 1]);
      if (ipv4Groups === null) return null;
      tail = tail.slice(0, -1);
      if (head.length + tail.length + 2 > 8) return null;
    } else {
      if (head.length + tail.length >= 8) return null;
    }
  }
  const groups = new Array(8).fill(0);
  for (let i = 0; i < head.length; i++) {
    const g = parseHexGroup(head[i]);
    if (g < 0) return null;
    groups[i] = g;
  }
  // Tail groups fill from the end; with an IPv4 tail the hex groups stop at
  // index 5 (groups 6-7 are the IPv4 part).
  const tailStart = ipv4Groups !== null ? 6 - tail.length : 8 - tail.length;
  for (let i = 0; i < tail.length; i++) {
    const g = parseHexGroup(tail[i]);
    if (g < 0) return null;
    groups[tailStart + i] = g;
  }
  if (ipv4Groups !== null) {
    groups[6] = ipv4Groups[0];
    groups[7] = ipv4Groups[1];
  }
  return groups;
}

// parseIPv4Embedded parses a dotted-quad IPv4 address (e.g. "0.0.0.0",
// "1.2.3.4") into two 16-bit groups (the high and low 16 bits), matching the
// tail of an IPv4-mapped/compatible IPv6 address. Returns null for anything
// that is not a valid dotted quad.
function parseIPv4Embedded(s: string): [number, number] | null {
  const octets = s.split(".");
  if (octets.length !== 4) return null;
  const vals: number[] = [];
  for (const o of octets) {
    if (o.length === 0 || !/^\d+$/.test(o)) return null;
    const v = parseInt(o, 10);
    if (v > 255) return null;
    vals.push(v);
  }
  return [(vals[0] << 8) | vals[1], (vals[2] << 8) | vals[3]];
}

// parseHexGroup parses a single 1-4 hex-digit IPv6 group (0–0xffff). Returns
// -1 for anything else so parseIPv6Groups can reject the whole address.
// parseInt(s, 16) silently stops at the first non-hex character (parseInt("1z", 16)
// === 1), so a malformed group like "1z" would be accepted as a valid group and
// canonicalize identically to a valid address (e.g. [2001:db8::1z] → [2001:db8::1]),
// suppressing a distinct future-state forecast. The entire group must match
// 1-4 hex digits before parsing.
function parseHexGroup(s: string): number {
  if (!/^[0-9a-fA-F]{1,4}$/.test(s)) return -1;
  return parseInt(s, 16);
}

// WELL_KNOWN_TCP_SERVICES maps the common TCP service names that
// net.LookupPort("tcp", ...) (and hence net.Listen) resolves from /etc/services,
// so a configured address like 0.0.0.0:http binds the same port a serving
// address reports as 0.0.0.0:80 or [::]:80. The browser cannot read /etc/services
// at runtime, so this mirrors the IANA well-known set Go's own lookup falls back
// to. The validator (config/listen_addr.go) accepts service-name ports via the
// same net.LookupPort call, so any name the config allows must resolve here for
// the dedup to match the configured spelling (channel 1) against the kernel's
// bound spelling (channel 2) — but only on a successful rebind (bindSucceeded),
// because Go's net.LookupPort augments and overwrites its built-in service
// mappings from the daemon host's service database: on a host mapping "http" to
// 8080, the table's "http"→80 is wrong, and a failed rebind leaves the serving
// port at the old address while the configured service name forecasts a different
// restart port, so the table would suppress a warning for a port the host may
// map differently. The resolved-port relaxation in listenerExposureDedup (also
// gated on bindSucceeded) handles a successful rebind without the table: on a
// successful rebind the configured port IS the serving port the kernel resolved
// it to, so a service-name forecast matches any concrete numeric serving port
// for the same listener and (normalized) host. Names this table does not carry
// (http-alt, mysql, …) are also handled by that relaxation. A name neither the
// table nor a successful rebind covers is returned unchanged so a name the
// server does not recognize never collapses two different addresses.
const WELL_KNOWN_TCP_SERVICES: ReadonlyMap<string, number> = new Map(
  // The entries below are the IANA well-known TCP services (port < 1024) that
  // /etc/services ships on Linux/macOS — the realistic set for a listen address.
  // This is not exhaustive, but covers every name net.Listen's address parser
  // would accept that a human might realistically write for a control-plane bind.
  Object.entries({
    "tcpmux": 1,
    "echo": 7,
    "discard": 9,
    "systat": 11,
    "daytime": 13,
    "netstat": 15,
    "qotd": 17,
    "chargen": 19,
    "ftp-data": 20,
    "ftp": 21,
    "ssh": 22,
    "telnet": 23,
    "smtp": 25,
    "time": 37,
    "whois": 43,
    "tacacs": 49,
    "domain": 53,
    "gopher": 70,
    "finger": 79,
    "http": 80,
    "kerberos": 88,
    "pop3": 110,
    "sunrpc": 111,
    "auth": 113,
    "nntp": 119,
    "netbios-ssn": 139,
    "imap2": 143,
    "snmp": 161,
    "snmp-trap": 162,
    "bgp": 179,
    "irc": 194,
    "ldap": 389,
    "https": 443,
    "microsoft-ds": 445,
    "submissions": 465,
    "exec": 512,
    "login": 513,
    "shell": 514,
    "printer": 515,
    "klogin": 543,
    "kshell": 544,
    "rdp": 3389,
  }),
);

// normalizeListenPort canonicalizes the port so zero-padded spellings ("00",
// "000") and an omitted port ("") — all of which net.LookupPort resolves to 0
// (the ephemeral wildcard) — compare equal to "0". A concrete port is returned
// as-is (leading zeros stripped for numeric strings). A non-numeric service name
// is resolved via the IANA well-known TCP services map
// (WELL_KNOWN_TCP_SERVICES) only on a successful rebind (bindSucceeded):
// Go's net.LookupPort augments its built-in service mappings from the daemon
// host's /etc/services, so the browser-side table is not authoritative on a
// host that maps a name to a different port. On a failed rebind (or a
// non-listener save) the serving address is the OLD bound port and the
// configured service name is a future state the kernel has not resolved, so
// resolving it to a well-known port would suppress a warning for a port the
// host may map differently. On a successful rebind the configured port IS the
// serving port, and the resolved-port relaxation in listenerExposureDedup
// (also gated on bindSucceeded) matches a service-name forecast to any
// concrete numeric serving port without the table, so the table is a
// convenience for the common case (http→80) rather than a source of truth.
// An unknown service name (which the validator would reject at config-set
// time, or the table does not carry) is returned unchanged so a name the
// server does not recognize never collapses two different addresses.
//
// Only a Go-style signed decimal port string is treated as numeric — Go's
// net.LookupPort parses decimal digits only (strconv.Atoi on the service-name
// fallback rejects hex and exponent spellings), so JavaScript's Number("0x50")
// (hex) or Number("1e2") (exponent) must not be coerced to a decimal port: on
// a host whose /etc/services maps "0x50" to a different port, the coercion
// would make a failed-rebind forecast (a future state the kernel has not
// resolved) compare equal to the old serving port and suppress a distinct
// warning. isDecimalPort (shared with listenerExposureDedup's relaxation)
// recognizes the same decimal grammar Go's port parser does.
function normalizeListenPort(port: string, bindSucceeded: boolean): string {
  if (isDecimalPort(port)) {
    return String(Number(port));
  }
  if (!bindSucceeded) {
    return port;
  }
  const resolved = WELL_KNOWN_TCP_SERVICES.get(port.toLowerCase());
  return resolved !== undefined ? String(resolved) : port;
}

// isDecimalPort reports whether a port string is a Go-style decimal port: a
// non-empty run of ASCII digits (optionally zero-padded), matching the grammar
// Go's net.LookupPort treats as a numeric port (strconv.Atoi, decimal only).
// The empty string (an omitted port, e.g. "0.0.0.0:") is also a decimal port —
// net.LookupPort resolves it to 0 (the ephemeral wildcard) — so it normalizes
// to "0" via normalizeListenPort. JavaScript's Number("0x50") (hex) and
// Number("1e2") (exponent) are NOT decimal ports: Go routes them through the
// service-name database (net.LookupPort), where /etc/services may map them to
// a different port than the hex/exponent value, so they must not be coerced to
// a decimal port in the browser.
function isDecimalPort(port: string): boolean {
  return port === "" || /^\d+$/.test(port);
}

// listenerRebindFailedFor reports whether a listener rebind failed during the
// apply, by checking the apply warnings (channel 2) for the rebind failure
// message the daemon's reconcile emits (listener_rebind.go:
// "apply network.listen_addr ..." / "apply network.preview_listen_addr ...").
// A failed rebind keeps the old listener serving, so the serving address channel
// 2 names is the OLD bound address, not the configured one — the dedup
// relaxations in listenerExposureDedup are unsafe (two different concrete
// addresses, not the same bind in a different form). The exposure notice
// ("... is reachable from the network ...") starts with the listener key, not
// "apply ", so it does not match.
//
// On a non-listener save (e.g. network.require_token) the per-key
// apply_outcome is "applied" for the saved key regardless of the listener
// rebind, because StatusForKey reports the saved key's status and the saved
// key is not a listener. The rebind failure warning in channel 2 is the only
// signal the response carries that the listener's rebind did not land, so the
// rebind status is derived from it rather than from the saved key.
function listenerRebindFailedFor(warnings: string[], listenerKey: string): boolean {
  const prefix = "apply " + listenerKey + " ";
  return warnings.some((w) => w.startsWith(prefix));
}

export function saveNotice(resp: ConfigSetResponse): string {
  const parts: string[] = [];
  const writeWarnings = resp.result.warnings ?? [];
  const applyWarnings = resp.warnings ?? [];
  // Whether the listener rebind the save moved actually landed — the daemon's
  // per-key apply_outcome. A listener key (network.listen_addr /
  // network.preview_listen_addr) whose rebind succeeded is "applied"; a failed
  // rebind is "deferred" (config/effect.go saveRules: ListenerRebindFailed →
  // ApplyStatusDeferred). Only a successful rebind guarantees the configured
  // address resolved to the serving address at bind time, so the hostname
  // relaxation in listenerExposureDedup (a non-literal configured host matched to
  // the literal serving IP) is gated on this. A require_token save (not a
  // listener key) is "applied" but no rebind happened, so the gate is the key AND
  // the outcome: a prior failed rebind left the listener serving a different
  // address than the configured one, and a require_token flip's forecast for the
  // configured address must not be deduped against the serving address's notice.
  //
  // A non-listener save can also retry a pending listener rebind (ApplyConfig
  // reconciles every save, so a temporarily occupied bind that failed on the
  // listener save can succeed when a require_token flip retried it). The
  // per-key apply_outcome for the saved key (e.g. network.require_token) is
  // "applied" because that key applied live; it does not reflect the listener
  // rebind, whose outcome the daemon reports only via a per-listener rebind
  // failure warning in channel 2 (applyWarnings). The rebind failure warning
  // (listener_rebind.go: "apply network.listen_addr ... — daemon still
  // serving ...") is absent when the rebind succeeded, so the per-listener
  // rebind status is derived from the apply result rather than only from the
  // saved key: a listener's rebind succeeded when apply_outcome is "applied"
  // and channel 2 does NOT carry a rebind failure warning for that listener
  // (the applyWarnings array is channel 2, which the daemon's reconcile
  // populates with the error on a failed rebind). This also covers the
  // listener-save case — a listener key whose rebind succeeded has
  // apply_outcome "applied" and no rebind failure warning, and one whose
  // rebind failed has apply_outcome "deferred" (so the gate fails on the
  // outcome before the rebind-failure check) and the warning.
  // Merge both warning channels so independent per-write advisories (e.g. an
  // unregistered-account warning on a default_accounts write) survive even when
  // channel 2 carries an unrelated apply warning (e.g. a pending rebind retried
  // on that same save). Two classes of duplication are suppressed after channel 2:
  //
  //   1. A write warning already in channel 2 verbatim. On a failed or
  //      unconfirmed apply the server's completeConfigSaveWarnings copies every
  //      result.warning into resp.warnings before returning, so an advisory the
  //      write produced would otherwise render twice (once per channel) the
  //      moment the apply also failed.
  //   2. The tokenless-exposure forecast (channel 1, exposureWarning) for a
  //      listener the apply also exposed (channel 2,
  //      ListenerExposureNotice/PreviewListenerExposureNotice). Both channels
  //      phrase that hazard differently and the server keeps channel 2
  //      authoritative on a successful exposure transition, so the forecast is
  //      dropped only when channel 2 carries the notice for the SAME listener at
  //      the SAME address — network.listen_addr and network.preview_listen_addr
  //      are independent listeners, and a notice for one is not an authoritative
  //      copy of a forecast for the other; and even for the same listener a
  //      failed rebind can leave the serving address (channel 2) different from
  //      the configured address (channel 1), in which case the forecast names a
  //      future state the notice does not cover. listenerExposureIdentity matches
  //      them, with relaxations: when the configured address is ephemeral
  //      (port :0), a successful rebind resolves it to a concrete kernel-assigned
  //      port that channel 2 names, so a :0 forecast matches any concrete serving
  //      port for the same listener and host (listenerExposureDedup); when the
  //      configured host is a non-literal hostname that a successful rebind
  //      resolved to the literal serving address, the browser cannot reproduce
  //      DNS, so the dedup matches by listener key and port alone when the rebind
  //      succeeded (gated on the per-key apply_outcome). The failed-rebind case
  //      has two different concrete addresses and is not relaxed.
  //
  // Other per-write advisories (the unregistered-account warning, the
  // project-scoped branch_prefix notice) have no apply-side counterpart and are
  // always preserved.
  const warnings = applyWarnings.length > 0
    ? [...applyWarnings, ...writeWarnings.filter((w) => {
        if (applyWarnings.includes(w)) {
          return false;
        }
        if (isListenerExposureWarning(w)) {
          const id = listenerExposureIdentity(w);
          // Derive the per-listener rebind status from the apply result for the
          // listener that this forecast describes, rather than only from the key
          // that initiated the save. A non-listener save (e.g.
          // network.require_token) with apply_outcome "applied" can also retry a
          // pending listener rebind: ApplyConfig reconciles every save, so a
          // bind that failed on the listener save (port occupied) can succeed on
          // the require_token flip that retried it. The per-key apply_outcome is
          // "applied" for the saved key but does not reflect the listener rebind,
          // whose outcome the daemon reports only via a rebind failure warning in
          // channel 2 (listener_rebind.go: "apply network.listen_addr ... —
          // daemon still serving on the previous address ..."). The absence of a
          // rebind failure warning for this listener in channel 2 means the
          // rebind either succeeded or was not needed (the listener is already at
          // the configured address), and in both cases the serving address IS the
          // resolved form of the configured address, so the dedup relaxations
          // (wildcard host, resolved port) are safe. The presence of a rebind
          // failure warning means the listener is still serving the old address,
          // so the configured address is a future state the kernel has not
          // resolved and the relaxations are unsafe (two different concrete
          // addresses, not the same bind in a different form).
          //
          // The hostname relaxation additionally requires rebindAttempted —
          // positive evidence that a rebind was actually attempted for this
          // listener on this save (the saved key is the listener key). A
          // non-listener save does not rebind the listener when the configured
          // address is unchanged: the daemon only calls bindWebLocked when the
          // configured address text differs from webConfigAddr, so a
          // non-listener save leaves the serving IP as the stale DNS resolution
          // from the previous bind. If DNS changed since that bind, channel 1's
          // hostname forecast names the hostname that will resolve to a different
          // IP after restart, so the hostname relaxation must not suppress it
          // without a confirmed rebind. rebindAttempted is the saved key being
          // the listener key, which means the daemon attempted a rebind for this
          // listener (a non-listener save does not, even if it retried a pending
          // rebind — the serving IP in that case is still the freshly resolved
          // one, but the browser cannot distinguish a retried rebind from a no-op
          // save, so the relaxation is gated on the saved key to be conservative
          // and surface the warning when the serving IP may be stale).
          //
          // For a listener key whose rebind succeeded, apply_outcome is "applied"
          // and no rebind failure warning is present, so the rebind is recognized
          // as succeeded here the same way. For a listener key whose rebind
          // failed, apply_outcome is "deferred" (config/effect.go saveRules:
          // ListenerRebindFailed → ApplyStatusDeferred), so the gate fails on the
          // outcome before the rebind-failure check.
          const wKey = id.indexOf(" ") >= 0 ? id.slice(0, id.indexOf(" ")) : "";
          const rebindSucceeded = resp.apply_outcome === "applied" && !listenerRebindFailedFor(applyWarnings, wKey);
          const rebindAttempted = resp.result.key === wKey;
          if (applyWarnings.some((aw) => isListenerExposureWarning(aw) && listenerExposureDedup(id, listenerExposureIdentity(aw), rebindSucceeded, rebindAttempted))) {
            return false;
          }
        }
        return true;
      })]
    : writeWarnings;
  // A warning makes the apply outcome actionable even for an ordinarily-live
  // key. In that case the restart notice must not be hidden by the key's static
  // requires-restart classification: it describes what happened to this save.
  if ((resp.result.requires_restart || warnings.length > 0) && resp.restart_notice !== "") {
    parts.push(resp.restart_notice);
  }
  parts.push(...warnings);
  const addr = resp.listener_addr ?? "";
  if (addr !== "") {
    parts.push(`Daemon now listening at ${addr}`);
  }
  return parts.join(" · ");
}

/**
 * Serializes async work per key: a second save for the same key waits for the
 * first to settle, so the LAST value the user chose is the last one written.
 *
 * WHY THE CLIENT AND NOT THE DAEMON. The daemon already owns the part it can
 * own: config.SetGlobalConfigValue takes a file lock, so concurrent writes
 * cannot corrupt or interleave, and that holds across the TUI, `af config set`,
 * and a hand-edit alike (config.toml is deliberately NOT daemon-exclusive state
 * — see configset.go). What a lock cannot supply is INTENT. Two in-flight POSTs
 * for the same key are ordered by the network, not by the user, so a checkbox
 * toggled on→off→on can land off — every write succeeded, none was clobbered,
 * and the answer is still wrong. Only the side that knows the user pressed off
 * before on can keep that order.
 *
 * Per KEY, not global: two different keys have no ordering relationship, and
 * queueing them behind each other would make a slow write block an unrelated one
 * for no gain.
 *
 * The returned promise settles when this item (and anything ahead of it for the
 * same key) settles. Production may ignore it; tests and other coordination
 * points can await real queue completion instead of guessing with a timer.
 */
export function createKeyedQueue(): (key: string, run: () => Promise<void>) => Promise<void> {
  const tails = new Map<string, Promise<void>>();
  return (key, run) => {
    const prev = tails.get(key) ?? Promise.resolve();
    // The chain must never reject, for two reasons: a failed save must not
    // cancel the next one (one rejected value would wedge that key for the
    // session), and a rejected tail with nothing queued behind it surfaces as an
    // unhandled rejection — a red error in the user's console every time the
    // validator refuses a value. The error is not swallowed: applyConfigValue's
    // own catch has already put the validator's message on screen. This only
    // keeps the QUEUE resolvable.
    const next = prev.then(run, run).catch(() => {});
    tails.set(key, next);
    void next.finally(() => {
      // Drop the entry only if nothing queued behind us, so the map stays the
      // size of the keys in flight rather than growing forever.
      if (tails.get(key) === next) {
        tails.delete(key);
      }
    });
    return next;
  };
}

export class ConfigPane {
  readonly el: HTMLElement;

  private entries: ConfigEntry[] = [];
  private registration = { open: false, agent: "" };
  private path = "";
  private status: ConfigStatus | null = null;
  /** The Accounts section's data (#3385). It is rendered by this view but is not
   *  config: see accounts.ts. */
  private accounts: AccountsState = emptyAccountsState();
  private showAdvanced = false;
  /** The key whose field is open, if any. Only one row edits at a time: a config
   *  write is per-key (like `af config set`), so a multi-row "save all" would
   *  imply an atomicity across keys that the writer does not offer. */
  private editing: string | null = null;
  private draft = "";
  // The live controls a rebuild replaces, so focus can be handed back to whichever of
  // them had it (#2933). Null whenever that control is not currently rendered.
  private editingInput: HTMLInputElement | null = null;
  /** The config key whose input held DOM focus when the in-progress rebuild started,
   *  so render() can re-point `editingInput` at that row's replacement even when the
   *  row is no longer the open edit. Live only for the duration of one render (set
   *  and cleared around the single `this.render()` call). */
  private restoreKey: string | null = null;
  private advancedToggle: HTMLElement | null = null;

  private lastEntries: ConfigEntry[] | null = null;
  private lastStatus: ConfigStatus | null = null;
  private lastAccounts: AccountsState | null = null;

  constructor(private readonly actions: ConfigActions) {
    this.el = h("section", { class: "af-config" });
    this.el.setAttribute("aria-label", "Config");
  }

  /** Feeds the pane fresh manifest rows. Re-rendering is skipped when nothing
   *  changed, matching the rest of the shell's patch-in-place model. */
  update(entries: ConfigEntry[], path: string, status: ConfigStatus | null, accounts: AccountsState): void {
    if (this.lastEntries === entries && this.lastStatus === status && this.lastAccounts === accounts) {
      return;
    }
    // Captured before `lastStatus` is overwritten below: it is the EDGE that
    // `shouldCloseSavedField` gates on, and by then the level would look unchanged.
    const statusIsNew = status !== this.lastStatus;
    const registrationSucceeded = accounts.status !== this.accounts.status && accounts.status
      && accounts.status.name === "" && !accounts.status.error;
    if (registrationSucceeded) {
      const submitted = this.accountInput(accounts.status!.agent);
      if (submitted) submitted.value = "";
    }
    this.lastEntries = entries;
    this.lastStatus = status;
    this.lastAccounts = accounts;
    // Retired web appearance keys from older daemons are never editable here.
    this.entries = entries.filter((entry) => entry.key !== "theme" && !entry.key.startsWith("theme."));
    this.path = path;
    this.status = status;
    this.accounts = accounts;
    if (shouldCloseSavedField(status, this.editing, statusIsNew)) {
      this.editing = null;
      this.draft = "";
    }
    this.rerenderKeepingUserState();
  }

  /**
   * Re-renders the pane without throwing away what the user was in the middle of.
   *
   * Every rebuild replaces the controls, which costs three things the render itself
   * cannot recover: the reader's scroll offset, focus, and the caret. The typed text
   * already survives (render reads `draft`); focus did not, and that is the one that
   * bites hardest — the app's document-level keys are gated on
   * `isNativeControl(document.activeElement)` (index.ts), so once focus falls back to
   * <body> the REST of what the user types stops being a value and starts being
   * shortcuts: a "[" or "]" in a path cycles the view out from under them mid-edit.
   *
   * Focus is handed back only to a control that genuinely HAD it, so a rebuild can
   * never steal focus from wherever the user actually is. Both re-render paths go
   * through here — the data update and the advanced-settings toggle — because a toggle
   * that drops its own focus cannot be operated twice from the keyboard.
   */
  private rerenderKeepingUserState(): void {
    const active = document.activeElement;
    // The account register fields (#3385) are rebuilt like everything else, and
    // they hold a name the user is part-way through typing. Capturing every
    // field's text — not only the focused one — is what keeps a refresh landing
    // from another action (a register elsewhere, a login closing) from eating a
    // draft the user never submitted.
    const accountDrafts = this.readAccountDrafts();
    const accountAgentFocused = active?.getAttribute("aria-label") === "Account agent";
    const accountSummaryFocused = active === this.el.querySelector(".af-account-disclosure summary");
    const focusedAccount =
      active instanceof HTMLInputElement ? active.getAttribute(ACCOUNT_INPUT_ATTR) : null;
    const accountCaret = focusedAccount !== null && active instanceof HTMLInputElement
      ? { start: active.selectionStart, end: active.selectionEnd }
      : null;
    const wasEditing = this.editingInput !== null && active === this.editingInput;
    // Both ends, not just the start: a rebuild landing while text is SELECTED would
    // otherwise silently collapse the selection the user was about to overwrite.
    const caretStart = wasEditing ? (this.editingInput?.selectionStart ?? null) : null;
    const caretEnd = wasEditing ? (this.editingInput?.selectionEnd ?? null) : null;
    const wasToggle = this.advancedToggle !== null && active === this.advancedToggle;
    // The config list is always the same list — one global manifest — so a rebuild
    // never legitimately starts at the top. Keeping the place matters most right after
    // a save, which is exactly when this fires.
    // Which ROW to hand focus back to is decided here, from the element that
    // actually holds it, and never from `status.key`. Saves are serialized per key
    // (`createKeyedQueue` in index.ts), so two keys in flight together can land out
    // of order: save A, move to B, save B, B answers first. A's later status would
    // name row A while the user is sitting in row B — and re-pointing by status key
    // would then move focus and the captured caret into A, so the next keystroke
    // edits the wrong setting. The key that held focus is the only thing that
    // answers "where was the user", which is the question restoration is asking.
    // aria-label carries it because render() sets it to `e.key` on every row input.
    this.restoreKey = wasEditing ? this.editingInput?.getAttribute("aria-label") ?? null : null;
    rebuildKeepingScroll(this.el, CONFIG_LIST_TOKEN, CONFIG_LIST_TOKEN, () => this.render());
    this.restoreKey = null;
    // preventScroll on BOTH: focus() scrolls its target into view by default, which
    // would undo the offset rebuildKeepingScroll just restored. That is not a corner
    // case — a user who wheels the pane while a field still holds focus is exactly the
    // reader this change exists for, and a plain focus() would yank them back to the
    // field the moment a save or an external refresh landed.
    this.restoreAccountDrafts(accountDrafts);
    if (wasEditing && this.editingInput) {
      this.editingInput.focus({ preventScroll: true });
      if (caretStart !== null) {
        this.editingInput.setSelectionRange(caretStart, caretEnd ?? caretStart);
      }
    } else if (wasToggle && this.advancedToggle) {
      this.advancedToggle.focus({ preventScroll: true });
    } else if (accountAgentFocused) {
      this.el.querySelector<HTMLElement>('[aria-label="Account agent"]')?.focus({ preventScroll: true });
    } else if (accountSummaryFocused) {
      this.el.querySelector<HTMLElement>(".af-account-disclosure summary")?.focus({ preventScroll: true });
    } else if (focusedAccount !== null) {
      const field = this.accountInput(focusedAccount);
      // preventScroll for the same reason as above: focus() would otherwise undo
      // the offset rebuildKeepingScroll just restored.
      field?.focus({ preventScroll: true });
      if (field && accountCaret?.start != null) {
        field.setSelectionRange(accountCaret.start, accountCaret.end ?? accountCaret.start);
      }
    }
  }

  /** Every register field's current text, keyed by agent. */
  private readAccountDrafts(): Map<string, string> {
    const drafts = new Map<string, string>();
    for (const field of this.el.querySelectorAll<HTMLInputElement>(`input[${ACCOUNT_INPUT_ATTR}]`)) {
      const agent = field.getAttribute(ACCOUNT_INPUT_ATTR);
      if (agent !== null && field.value !== "") {
        drafts.set(agent, field.value);
      }
    }
    return drafts;
  }

  /** Puts them back on the freshly rendered fields. An agent whose row is gone —
   *  a roster that shrank between renders — simply drops its draft; there is no
   *  field left to hold it. */
  private restoreAccountDrafts(drafts: Map<string, string>): void {
    for (const [agent, value] of drafts) {
      const field = this.accountInput(agent);
      if (field) {
        field.value = value;
      }
    }
  }

  private accountInput(agent: string): HTMLInputElement | null {
    return this.el.querySelector<HTMLInputElement>(
      `input[${ACCOUNT_INPUT_ATTR}="${CSS.escape(agent)}"]`,
    );
  }

  private render(): void {
    // Dropped first: every rebuild replaces the controls, and a handle to a previous
    // one would name a detached node for the rest of this render.
    this.editingInput = null;
    this.advancedToggle = null;
    const head = h(
      "div",
      { class: "af-config-head" },
      h("span", { class: "af-config-title" }, "Config"),
      h("span", { class: "af-view-count" }, String(this.entries.length)),
    );
    if (this.path !== "") {
      // Name the file being edited: a user with AF_HOME set is otherwise left
      // guessing which config.toml this is.
      head.append(h("span", { class: "af-config-path" }, `Daemon ${location.host} · ${this.path}`));
    }
    // The conversational assistant (#2467): a chat that helps configure the service,
    // the web counterpart of the TUI's config-agent takeover. Opening it is the
    // shell's job (it owns the token + modal host), so the button only reports intent.
    const assistantBtn = h(
      "button",
      { type: "button", class: "af-ghost af-config-assistant-btn" },
      "Configure with assistant",
    );
    assistantBtn.addEventListener("click", () => this.actions.openAssistant());
    head.append(assistantBtn);

    const sections: HTMLElement[] = [];
    for (const { tier, name } of tiersInOrder(this.entries)) {
      const inTier = this.entries.filter((e) => e.tier === tier);
      if (inTier.length === 0) {
        continue;
      }
      const folded = tier === TIER_ADVANCED && !this.showAdvanced;
      const heading = h("div", { class: "af-config-tier" }, h("span", { class: "af-config-tier-name" }, name.charAt(0).toUpperCase() + name.slice(1)));

      if (tier === TIER_ADVANCED) {
        const toggle = h(
          "button",
          { type: "button", class: "af-ghost af-config-toggle" },
          folded ? `Show ${inTier.length} advanced settings` : "Hide advanced settings",
        );
        toggle.addEventListener("click", () => {
          this.showAdvanced = !this.showAdvanced;
          this.rerenderKeepingUserState();
        });
        this.advancedToggle = toggle;
        heading.append(toggle);
      }
      sections.push(heading);
      if (folded) {
        continue;
      }
      for (const e of inTier) {
        sections.push(this.renderRow(e));
      }
    }

    const content =
      sections.length > 0
        ? sections
        : [
            h(
              "p",
              { class: "af-config-empty" },
              "No settings available. Try Configure with assistant.",
            ),
          ];
    // Accounts LAST: the config keys are what this view is for, and a credential
    // section above them would push them below the fold.
    this.el.replaceChildren(
      head,
      h("div", { class: "af-config-list" }, ...content),
      renderAccountsSection(this.accounts, this.actions.accounts, this.registration),
    );
  }

  /** One key: its name, purpose, control, and — when it is the row just written
   *  or just refused — the echo or the error. */
  private renderRow(e: ConfigEntry): HTMLElement {
    const row = h("div", { class: "af-config-row" });
    row.setAttribute("data-key", e.key);

    const label = h(
      "div",
      { class: "af-config-label" },
      h("span", { class: "af-config-key" }, e.key),
      h("span", { class: "af-config-purpose" }, e.purpose),
    );
    row.append(label);
    row.append(this.renderControl(e));

    const status = this.status;
    if (status && status.key === e.key) {
      if (status.error !== "") {
        // The validator's message, verbatim — never reworded here.
        row.append(h("div", { class: "af-config-error", role: "alert" }, status.error));
      } else {
        row.append(h("div", { class: "af-config-echo" }, `set ${status.key} = ${status.value}`));
        if (status.notice !== "") {
          // The restart notice, at the moment of the edit.
          row.append(h("div", { class: "af-config-notice" }, status.notice));
        }
      }
    }
    return row;
  }

  /** The control for one key, chosen from the manifest's own description of it:
   *  a picker when the values are enumerated, a checkbox for a bool, and a text
   *  field otherwise. Structured rows use the compact JSON CurrentValue emits.
   *
   *  The enum and type come from the manifest; Go-side coverage separately pins
   *  every global row to the real writer so a rendered field cannot dead-end at
   *  a save the writer refuses. */
  private renderControl(e: ConfigEntry): HTMLElement {
    const kind = controlKind(e);

    if (kind === "checkbox") {
      const box = h("input", { type: "checkbox", class: "af-config-check" });
      box.checked = e.value === "true";
      box.setAttribute("aria-label", e.key);
      // A checkbox has no separate commit gesture: toggling IS the edit.
      box.addEventListener("change", () => this.actions.save(e.key, box.checked ? "true" : "false"));
      return h("div", { class: "af-config-control" }, box);
    }

    if (kind === "select") {
      const select = h("select", { class: "af-input af-config-input" });
      select.setAttribute("aria-label", e.key);
      // controlKind only returns "select" for a non-empty enum, but narrow it
      // explicitly rather than asserting: the compiler is the cheapest place to
      // keep the two in agreement.
      for (const v of e.enum ?? []) {
        const opt = h("option", { value: v }, v);
        if (v === e.value) {
          opt.selected = true;
        }
        select.append(opt);
      }
      select.addEventListener("change", () => this.actions.save(e.key, select.value));
      return h("div", { class: "af-config-control" }, select);
    }

    const input = h("input", { type: "text", class: "af-input af-config-input", autocomplete: "off" });
    input.value = this.editing === e.key ? this.draft : e.value;
    if (this.editing === e.key) {
      this.editingInput = input;
    }
    // A same-key Enter-save closes this field (update() clears `editing` before the
    // rerender), so the gate above does not re-point `editingInput` at the rebuilt
    // input — and `rerenderKeepingUserState`'s `if (wasEditing && this.editingInput)`
    // restoration then no-ops, dropping focus to <body>. The user's next keystrokes
    // become document shortcuts (`[`/`]` cycle the view) or, for any other printable
    // key, are silently swallowed (`decideKey` returns `{kind:"none"}` without
    // preventDefault for a body-focused key in config view). #2955's gate covered
    // only the unrelated-rebuild case; the same-key variation was never analyzed.
    //
    // Keep the handle live for the row that HELD FOCUS so the existing restoration
    // branch hands focus (and the caret) back to its replacement. `restoreKey` is
    // read rather than `status.key` because the two diverge exactly when it matters
    // — see rerenderKeepingUserState. The `=== null` guard means an actively-edited
    // row's capture above always wins. This re-points the handle on every rebuild
    // that finds the row still focused, including the one the post-save refetch
    // triggers, so focus survives both renders. It does not re-open the edit:
    // `editing` stays null and the Save button stays disabled via `canCommit`
    // against the refreshed `e.value`.
    if (this.restoreKey === e.key && this.editingInput === null) {
      this.editingInput = input;
    }
    input.setAttribute("aria-label", e.key);

    const save = h("button", { type: "button", class: "af-primary af-config-save" }, "Save");

    const dirty = h("span", { class: "af-config-dirty", role: "status" }, "Unsaved");

    // ONE gate, both gestures. There are two ways to commit this field — the
    // button and Enter — and they must agree. Enter used to call save()
    // unconditionally while the button honored `disabled`, so pressing Enter on
    // an untouched field wrote it anyway: an echo and a restart notice for a
    // no-op, telling the user something happened when nothing did. Routing both
    // through commit() is what keeps a disabled control actually disabled.
    const commit = () => {
      if (!canCommit(input.value, e.value)) {
        return;
      }
      this.actions.save(e.key, input.value);
    };

    const syncSave = () => {
      save.disabled = !canCommit(input.value, e.value);
      dirty.hidden = save.disabled;
    };

    input.addEventListener("input", () => {
      this.editing = e.key;
      this.draft = input.value;
      this.editingInput = input;
      syncSave();
    });
    input.addEventListener("keydown", (ev: KeyboardEvent) => {
      if (ev.key === "Enter") {
        ev.preventDefault();
        commit();
      }
    });
    save.addEventListener("click", commit);
    syncSave();

    return h("div", { class: "af-config-control" }, input, dirty, save);
  }
}
