#!/usr/bin/env bash
# gemini-bundle-env-measure.sh — emit every environment variable name the
# code bundled into @google/gemini-cli@<GeminiBundleVersion> reads, one per
# line, sorted. This is the MEASUREMENT behind
# internal/sessionenv/gemini_bundle_env.go: the artifact claims or waives
# each emitted name, and a test fails when the two sets disagree.
#
# The package is fetched with `npm pack` into a fresh empty directory and
# READ with grep only — it is never installed, run or required (#7662).
# The version is read out of the artifact's own GeminiBundleVersion const
# so the measurement and the claim cannot drift silently.
#
# Usage:
#   scripts/gemini-bundle-env-measure.sh           # emit to stdout
#   scripts/gemini-bundle-env-measure.sh --update  # rewrite the committed list
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
VERSION="$(sed -n 's/^const GeminiBundleVersion = "\(.*\)"$/\1/p' \
	"$REPO_ROOT/internal/sessionenv/gemini_bundle_env.go" | head -1)"
: "${VERSION:?GeminiBundleVersion constant not found in gemini_bundle_env.go}"

LIST_FILE="$REPO_ROOT/internal/sessionenv/gemini_bundle_env_measured.txt"

dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
# npm pack only downloads the tarball into the EMPTY directory — no install,
# no lifecycle scripts, no require.
(cd "$dir" && npm pack "@google/gemini-cli@$VERSION" >/dev/null && tar xzf ./*.tgz)
pkg="$dir/package"
[ -d "$pkg/bundle" ] || { echo "bundle/ missing in @google/gemini-cli@$VERSION tarball" >&2; exit 1; }
# Only the code — bundle/docs holds prose that mentions env names without
# reading them.
codefiles="$(find "$pkg/bundle" -type f \( -name '*.js' -o -name '*.mjs' \) -print)"
: "${codefiles:?no code files under bundle/}"

# Every read shape measured in the 0.42.0 bundle:
#   process*.env.NAME         direct dot read (bundler renames: process2,
#                             process13, process20)
#   process*.env["NAME"]      direct bracket read, either quote style
#   env*["NAME"] / env*.NAME  reads through an environment ALIAS — env2 =
#                             process.env, and options.env callers pass
#                             process.env (resolveTelemetrySettings)
#   getEnv("NAME")            genai's process.env[env]?.trim() accessor and
#   get{String,Number,Boolean}FromEnv("NAME")   the vendored OTEL accessors
#   parseBooleanEnvFlag(env2["NAME"])           covered by the env* rule above
#   "NAME" in env*            presence reads (CI-detection family)
#   [...].some(s => s in env) presence-scan arrays (the vendored is-ci copy):
#                             the member literals ride the same line
#   process*.env[CONST]       constant-named reads, resolved through the
#   env*[CONST]               bundle's own `CONST = "NAME"` definitions
emit() {
	{
		# process*.env.NAME
		grep -rhoE 'process[0-9]*\.env\.[A-Za-z_$][A-Za-z0-9_$]*' $codefiles \
			| sed -E 's/^process[0-9]*\.env\.//'
		# process*.env["NAME"] / process*.env['NAME']
		grep -rhoE 'process[0-9]*\.env\["[A-Za-z_$][A-Za-z0-9_$]*"\]' $codefiles \
			| sed -E 's/.*\["//; s/"\]$//'
		grep -rhoE "process[0-9]*\.env\['[A-Za-z_\$][A-Za-z0-9_\$]*'\]" $codefiles \
			| sed -E "s/.*\['//; s/'\]\$//"
		# env*["NAME"] / env*['NAME'] / env*.NAME / env*[`NAME`]
		grep -rhoE '\benv[0-9]*\["[A-Za-z_$][A-Za-z0-9_$]*"\]' $codefiles \
			| sed -E 's/.*\["//; s/"\]$//'
		grep -rhoE "\benv[0-9]*\['[A-Za-z_\$][A-Za-z0-9_\$]*'\]" $codefiles \
			| sed -E "s/.*\['//; s/'\]\$//"
		grep -rhoE '\benv[0-9]*\.[A-Z_][A-Z_0-9]*\b' $codefiles \
			| sed -E 's/^env[0-9]*\.//'
		# env*[`NAME`] — a backtick literal with no ${} substitution is a
		# plain string read (process.env[`OTEL_EXPORTER_OTLP_INSECURE`])
		grep -rhoE '(process[0-9]*\.env|\benv[0-9]*)\[`[A-Za-z_$][A-Za-z0-9_$]*`\]' $codefiles \
			| sed -E 's/.*\[`//; s/`\]$//'
		# literal-string accessor calls, either quote style
		grep -rhoE '\b(getEnv|getStringFromEnv|getNumberFromEnv|getBooleanFromEnv|getNumberFromEnvOrThrow|getStringFromEnvOrThrow|parseBooleanEnvFlag)\("[A-Za-z_$][A-Za-z0-9_$]*"\)' $codefiles \
			| sed -E 's/^[A-Za-z_]+\("//; s/"\)$//'
		grep -rhoE "\b(getEnv|getStringFromEnv|getNumberFromEnv|getBooleanFromEnv|getNumberFromEnvOrThrow|getStringFromEnvOrThrow|parseBooleanEnvFlag)\('[A-Za-z_\$][A-Za-z0-9_\$]*'\)" $codefiles \
			| sed -E "s/^[A-Za-z_]+\('//; s/'\)\$//"
		# "NAME" in env — presence reads
		grep -rhoE '"[A-Za-z_$][A-Za-z0-9_$]*" in env[0-9]*\b' $codefiles \
			| sed -E 's/^"//; s/" in .*$//'
		# presence-scan arrays: every "NAME" literal on a line that runs a
		# `x in env` membership test pulls the array members out with it
		grep -rhE '\bin env[0-9]*\b' $codefiles | grep -oE '"[A-Z_][A-Z_0-9]*"' | tr -d '"'
		# constant-named reads: process*.env[CONST] / env*[CONST] /
		# process.env[exports.CONST] resolved through the bundle's own
		# `CONST = "VALUE"` definitions
		for ident in $(grep -rhoE '(process[0-9]*\.env|\benv[0-9]*)\[[A-Za-z_0-9.]*[A-Z_][A-Z_0-9]*\]' $codefiles \
			| sed -E 's/.*\[(.*)\]/\1/; s/.*\.//' | sort -u); do
			grep -rhoE "\\b${ident}\\s*=\\s*\"[^\"]+\"" $codefiles | sed -E 's/.*=\s*"//; s/"$//'
		done
		# the third-party proxy helper's proto-expanded head:
		# getEnv('npm_config_' + proto + '_proxy') for the http(s) URLs it
		# resolves
		if grep -qF "npm_config_' + proto + '_proxy" "$pkg/bundle/bundled/third_party/index.js" 2>/dev/null; then
			printf '%s\n' npm_config_http_proxy npm_config_https_proxy \
				NPM_CONFIG_HTTP_PROXY NPM_CONFIG_HTTPS_PROXY
		fi
		# that helper's getEnv reads lower||UPPER for every literal, so its
		# spelled names land in both cases
		grep -rhoE "getEnv\('[a-z_0-9]+'\)" "$pkg/bundle/bundled/third_party/index.js" 2>/dev/null \
			| sed -E "s/getEnv\('//; s/'\)//" | sort -u \
			| while read -r n; do printf '%s\n%s\n' "$n" "$(printf %s "$n" | tr '[:lower:]' '[:upper:]')"; done
		# every literal OTEL_EXPORTER_OTLP_* name — the vendored SDK reads
		# shared and per-signal spellings directly
		grep -rhoE 'OTEL_EXPORTER_OTLP_[A-Z_]+' $codefiles
		# OTEL per-signal template reads expand over the signal identifiers
		# the SDK names: TRACES, METRICS, LOGS
		for suffix in $(grep -rhoE 'OTEL_EXPORTER_OTLP_\$\{signalIdentifier\}_[A-Z_]+' $codefiles \
			| sed -E 's/.*\$\{signalIdentifier\}_//' | sort -u); do
			for sig in TRACES METRICS LOGS; do
				printf 'OTEL_EXPORTER_OTLP_%s_%s\n' "$sig" "$suffix"
			done
		done
		# key-template families: a marker names the prefix, not a member
		grep -rlqF 'GEMINI_PROMPT_${' $codefiles && echo 'GEMINI_PROMPT_*'
		grep -rlqF 'Object.keys(process.env)' $codefiles \
			&& grep -rlqF '/^debug_/i' $codefiles && echo 'debug_*'
	} | sort -u
}

emitted="$(emit)"

if [ "${1:-}" = "--update" ]; then
	printf '%s\n' "$emitted" > "$LIST_FILE"
	echo "wrote $(printf '%s\n' "$emitted" | wc -l) names to ${LIST_FILE#"$REPO_ROOT"/} (measured @google/gemini-cli@$VERSION)" >&2
else
	printf '%s\n' "$emitted"
fi
