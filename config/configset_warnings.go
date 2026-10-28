package config

// listenerWriteRefusal names the one `af config` refusal that is not a syntax
// or drift problem: a write that would LEAVE the control-plane listener in the
// #5137 refused posture — non-loopback listen_addr, require_token false, and
// no allow_unauthenticated_network opt-in.
//
// cfg is the config the edit RESULTS in — parsed from the bytes about to be
// written inside the file lock — so the judgment covers the pairing, never one
// key alone (#2412: two racing writers can each turn on one half of the
// exposure). A write that lands the refused combination is refused with the
// SAME message the daemon would emit at bind (ListenerBindRefusal), and the
// file is left untouched: refusing the write is refusing to create a config
// the daemon must then refuse to serve.
//
// The gate is scoped to the two keys a write can use to CREATE the refused
// posture — network.listen_addr pointed at the network, and require_token
// turned off. An unrelated write on an already-refused config (a hand-edit
// left one) is allowed: it did not create the posture, it leaves it no worse,
// and refusing it would strand every other config edit behind an unrelated
// fix — while every remediation write lands a safe resulting config and is
// therefore allowed.
//
// network.allow_unauthenticated_network is deliberately NOT gated. The only
// write of it that lands on a refused resulting posture is the REVOCATION —
// setting it false, or unsetting it, under a non-loopback tokenless bind —
// and refusing that write would keep the very listener it is revoking serving
// unauthenticated. `af doctor`'s own remediation for an opted-in exposure is
// `af config set network.allow_unauthenticated_network false`; it must land so
// the daemon's next apply can retire the listener into the refused state the
// operator just asked for.
//
// network.require_loopback_token is deliberately absent too: it is inert while
// require_token is false (tokenDisabled short-circuits the gate), so it neither
// creates nor rescues the exposure. network.preview_listen_addr is a separate
// listener that never serves the control plane, so it is governed by its own
// notice (PreviewListenerExposureNotice) and stays warning-only.
func listenerWriteRefusal(key string, resulting *Config) string {
	switch canonicalConfigKey(key) {
	case "network.listen_addr", "network.require_token":
		return ListenerBindRefusal(resulting)
	}
	return ""
}

// exposureWarning returns a warning when cfg — the config that RESULTS from
// this set, parsed from the bytes about to be written — serves an
// unauthenticated control plane to the network, i.e. the combination of a
// non-loopback listen_addr and require_token = false.
//
// cfg is the OUTCOME, not the starting point: it already carries the value
// being written, so nothing is spliced in here. That is deliberate (#2412) —
// splicing meant taking the other half of the pairing from a config loaded
// before the file lock, which two racing writers could each read stale. See
// scalarWrite.apply.
//
// Since #5137 this warning only ever describes an OPTED-IN exposure: a write
// whose result would be refused outright never reaches it — listenerWriteRefusal
// already refused that write with the shared remediation message. What is left
// for the warning is the combination that IS going to serve unauthenticated —
// non-loopback listen_addr, token off, and allow_unauthenticated_network = true
// — so the operator sees the risk they just accepted stated plainly, once.
//
// Both directions of the pairing warn, because either key can create the
// exposure: pointing listen_addr at the network while the token is off, or
// turning the token off while listen_addr is already on the network. Setting
// any OTHER key stays silent even on an already-exposed config — this speaks to
// the change the user just made, and warning on every unrelated `config set`
// would train them to ignore it. Setting network.allow_unauthenticated_network
// itself stays silent too: opting in IS the acknowledgement, so warning about
// it would be noise.
//
// network.preview_listen_addr is the parallel surface for the web-tab preview
// listener (#1856). It has NO "other half" pairing: the preview origin never
// serves the control plane, and its own credential is the per-tab hostname, so
// the notice depends on preview_listen_addr ALONE and does NOT gate on
// network.require_token (see PreviewListenerExposureNotice). That notice replaces
// the control-plane text and the ListenerServesUnauthenticatedNetwork gate for
// this case, and warns whenever the resulting preview bind is network-reachable
// — matching this emitter's "warn on every exposed write of the changed key"
// discipline, so an operator moving an ALREADY network-bound preview between
// two non-loopback addresses is still warned (the apply-time transition emitter
// in daemon/config_apply.go handles the once-per-transition INTO exposure, and
// is not transition-gated for "still exposed, just moving"). The flat alias
// spelling "preview_listen_addr" canonicalizes to "network.preview_listen_addr"
// before the switch, so both CLI spellings warn.
//
// The exposure test is ListenerServesUnauthenticatedNetwork — the SAME predicate
// the daemon's refusal builds on, itself built on the IsLoopbackListenAddr the
// token gate derives from. Two definitions of "is this exposed" drifting apart
// is precisely how a security check rots, so there is only one.
func exposureWarning(cfg *Config, key string) string {
	if cfg == nil {
		return ""
	}
	key = canonicalConfigKey(key)
	switch key {
	case "network.listen_addr", "network.require_token":
		return ListenerExposureNotice(cfg)
	case "network.preview_listen_addr":
		// PreviewListenerExposureNotice is itself non-transition-gated: it returns
		// a notice whenever cfg.PreviewListenAddr is non-empty and non-loopback,
		// so a write that moves an already-exposed preview between two non-loopback
		// addresses still warns — parity with the control-plane case above, which
		// warns on every exposed write of the changed key.
		return PreviewListenerExposureNotice(cfg)
	default:
		return ""
	}
}
