package session

// The credential-mount path's half of the #7662 bundle artifact: the two
// gemini-specific tables below derive from
// internal/sessionenv.GeminiBundleEnvReads (@google/gemini-cli 0.42.0) so the
// mount path cannot drift from the measured list — a bundle read the audit
// classifies reaches this path's denied set and its pin or re-assert through
// the same entries the account path consumes.

import "github.com/sachiniyer/agent-factory/internal/sessionenv"

// geminiBundleLoaderFilled is the mount path's .env-fillable set for
// gemini: every artifact entry whose classification is keep-if-set — the
// names the CLI's own dotenv loader can claim when unset. af denies each to
// repo run_args and pins it defined-and-empty unless the operator forwarded
// it, so the loader finds the name claimed and the measured read falls back
// to its own default. Names that also sit in the shared or conditional
// tables (proxies, the project chain, GITHUB_TOKEN) land in both — the
// denied set is a union, so the classes cannot disagree.
func geminiBundleLoaderFilled() map[string]struct{} {
	out := make(map[string]struct{})
	for _, r := range sessionenv.GeminiBundleEnvReads {
		if r.Account == sessionenv.GeminiBundleEnvKeepIfSet {
			out[r.Name] = struct{}{}
		}
	}
	return out
}

// geminiBundleFirstPartyReasserts is the mount path's valued-claim set for
// gemini: every artifact entry whose mount class is reassert — the measured
// ??-family reads where defined-empty is a real (broken) value, so af
// installs the first-party default after run_args instead of an empty pin.
// Entries reasserted through the boundary's own tables (HOME, PATH, SHELL,
// the XDG roots, the editor pair) carry no Value here — they keep their
// existing mechanism.
func geminiBundleFirstPartyReasserts() map[string]string {
	out := make(map[string]string)
	for _, r := range sessionenv.GeminiBundleEnvReads {
		if r.Mount == sessionenv.GeminiBundleEnvReassert && r.Value != "" {
			out[r.Name] = r.Value
		}
	}
	return out
}
