// Package sessionenv — the checked completeness artifact for issue #7662.
//
// GeminiBundleEnvReads is every environment name the code bundled into
// @google/gemini-cli@GeminiBundleVersion reads from process.env — its own
// source AND the bundled transports (grpc-js, google-auth-library,
// gcp-metadata, googleapis-common, @google-cloud/logging, the
// chrome-devtools-mcp bundle and the npm-config proxy helper), including
// reads made through aliases of the environment (env2 = process.env,
// options.env whose callers pass process.env) — measured with
// scripts/gemini-bundle-env-measure.sh, which npm-packs the CLI into an
// empty directory and greps the bundle without installing, running or
// requiring it. The script's emitted list is committed beside this file as
// gemini_bundle_env_measured.txt, and the test pair to it fails when the
// artifact and the measurement disagree.
//
// The scope of the CLAIM is narrow on purpose (#7665 review): only names
// that select the proxy, the trust roots, the credential or the project of
// a request that carries the mounted credential — plus every link of those
// reads' fallback chains — are pinned, re-asserted or refused. Every other
// read stays enumerated here as a WAIVER with its reason, so the artifact
// remains the complete enumeration without charging users for names that
// never touch the credential.
//
// Classes (per name, per path):
//   - pin      defined-and-empty; empty is inert at the measured read
//   - keep-if-set  account-path hold: a present (forwarded) value is kept,
//     an absent one is installed defined-empty
//   - reassert install the first-party default; defined-empty is a real
//     (broken) value at the measured ?? read
//   - account-root  af's injected account scope, never pinned
//   - shared-boundary / denied-no-pin / kept  claimed by the pre-existing
//     shared or allowlist tables — recorded so the enumeration is
//     complete without duplicating a hold owned elsewhere
//   - waived   reviewed and deliberately unclaimed; Reason required
package sessionenv

const GeminiBundleVersion = "0.42.0"

// GeminiBundleEnvClass is the hold af takes on one measured read, per
// defended path.
type GeminiBundleEnvClass string

const (
	// GeminiBundleEnvPin claims the name DEFINED-AND-EMPTY: empty is inert
	// at every measured read (each is a || / !! / === / truthy check that
	// falls back to the compiled default), so the .env loader finds the
	// name claimed while the agent sees its own default. On the mount path
	// the pin holds unless the operator forwarded the name in
	// session_env_passthrough; on the account path it is unconditional —
	// an ambient value there is repo-influenced (image ENV, run_args), the
	// same reasoning accountLateEnvironmentNames documents (#7531).
	GeminiBundleEnvPin GeminiBundleEnvClass = "pin"
	// GeminiBundleEnvKeepIfSet is the account path's hold for names whose
	// value may be deliberate operator deployment config: a present value
	// is kept (the env reaching the boundary is filtered, so presence is
	// the operator's claim), an absent one is installed defined-empty.
	// These are the accountLoaderFilledNames members; on the mount path
	// the same name is denied to run_args and pinned empty unless
	// forwarded.
	GeminiBundleEnvKeepIfSet GeminiBundleEnvClass = "keep-if-set"
	// GeminiBundleEnvReassert re-defines the name with the first-party
	// default Value on both paths: the read treats defined-empty as a real
	// (broken) value — a ?? nullish coalesce — so the claim must carry a
	// working value, not an empty pin. A re-assert never carries an
	// operator-overridable or version-specific constant (#7665 review).
	GeminiBundleEnvReassert GeminiBundleEnvClass = "reassert"
	// GeminiBundleEnvRoot is the account-scoped credential root af injects
	// (the account's config directory). Never pinned — pinning it empty
	// would erase the selection.
	GeminiBundleEnvRoot GeminiBundleEnvClass = "account-root"
	// GeminiBundleEnvShared is claimed by the SHARED, agent-agnostic
	// boundary tables rather than the artifact-derived sets: the mount
	// path's credentialMountReassertedNames /
	// credentialMountSharedCredentialNames /
	// credentialMountSharedRoutingNames /
	// credentialMountSharedProgramEntries deny or re-assert these names
	// with holds the artifact cannot express per agent. Listing the class
	// here keeps the artifact's claim complete WITHOUT making the derived
	// sets pretend they own it.
	GeminiBundleEnvShared GeminiBundleEnvClass = "shared-boundary"
	// GeminiBundleEnvKept is ambient configuration af deliberately keeps on
	// the recorded path — common-allowlist names and the conditional
	// cloud-credential group that need no pin.
	GeminiBundleEnvKept GeminiBundleEnvClass = "kept"
	// GeminiBundleEnvNoPin is refused to repository run_args but NEVER
	// pinned: the pane boundary itself installs the real value and a
	// defined-empty pin would erase it after tmux stamped it (#7531).
	GeminiBundleEnvNoPin GeminiBundleEnvClass = "denied-no-pin"
	// GeminiBundleEnvWaived is reviewed and deliberately unclaimed; Reason
	// carries the one-line justification. The test requires it.
	GeminiBundleEnvWaived GeminiBundleEnvClass = "waived"
)

// GeminiBundleEnvRead is one measured bundle read and its classification on
// each defended path.
type GeminiBundleEnvRead struct {
	Name    string
	Mount   GeminiBundleEnvClass // claim on the credential-mount path
	Account GeminiBundleEnvClass // claim on the account-scoped path
	// Value is the first-party default installed for Reassert classes.
	Value string
	// Read is the measured read (the expression or chain), as the bundle
	// performs it — the evidence the PR body shows.
	Read string
	// Reason is the one-line justification a Waived entry must carry.
	Reason string
}

// GeminiBundleEnvReads is the measured list — every name in
// gemini_bundle_env_measured.txt, classified in scope or waived.
var GeminiBundleEnvReads = []GeminiBundleEnvRead{
	// ── Proxy selection ────────────────────────────────────────────────
	// grpc-js's mapProxyName reads grpc_proxy FIRST, then https_proxy,
	// then http_proxy (chunk-DN4XSYRG.js:33980); the bypass is
	// no_grpc_proxy || no_proxy. The generic spellings route the same
	// credential-bearing traffic for undici and the tools the agent runs.
	// Every read is truthy/|| so defined-empty is inert; the account path
	// keeps a forwarded operator proxy.
	{Name: "grpc_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["grpc_proxy"] || process.env["https_proxy"] || process.env["http_proxy"] (grpc-js mapProxyName, chunk-DN4XSYRG.js:33980) — chain HEAD`},
	{Name: "no_grpc_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["no_grpc_proxy"] || process.env["no_proxy"] (grpc-js bypass chain head)`},
	{Name: "HTTP_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["HTTP_PROXY"]/` + "`HTTP_PROXY`" + ` — generic proxy; grpc-js chain tail's case twin`},
	{Name: "http_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["http_proxy"] — grpc-js chain tail link`},
	{Name: "HTTPS_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["HTTPS_PROXY"]`},
	{Name: "https_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["https_proxy"] — grpc-js chain middle link; undici reads it ahead of the uppercase spelling`},
	{Name: "ALL_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["ALL_PROXY"]`},
	{Name: "all_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `getEnv('all_proxy') — the all-protocol proxy name; its getEnv case-pair reads ALL_PROXY too`},
	{Name: "NO_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["NO_PROXY"]`},
	{Name: "no_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["no_proxy"] — grpc-js bypass chain tail link`},

	// ── Trust roots / credential-bearing transport ─────────────────────
	{Name: "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `var DEFAULT_ROOTS_FILE_PATH = process.env.GRPC_DEFAULT_SSL_ROOTS_FILE_PATH; if (DEFAULT_ROOTS_FILE_PATH) → createSecureContext ca (grpc-js, chunk-7VVHSNDQ.js:19800) — empty is falsy, falls back to compiled roots`},
	{Name: "GRPC_SSL_CIPHER_SUITES", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `exports.CIPHER_SUITES = process.env.GRPC_SSL_CIPHER_SUITES → createSecureContext ciphers (same file); measured: ciphers:"" is accepted — empty is inert`},
	{Name: "GOOGLE_API_CERTIFICATE_CONFIG", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `const envPath = process.env[exports.CERTIFICATE_CONFIGURATION_ENV_VARIABLE]; if (envPath ...) isValidFile(envPath) → mTLS client-cert config for the credentialed transport (google-auth-library, chunk-ECNYAST2.js:21464)`},

	// ── Credential / auth-source selection ─────────────────────────────
	// google_application_credentials is the LOWERCASE twin behind the
	// pinned uppercase name: google-auth-library reads
	// GOOGLE_APPLICATION_CREDENTIALS || google_application_credentials, so
	// the uppercase pin alone left the chain's second link free (#7662 R2).
	{Name: "google_application_credentials", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GOOGLE_APPLICATION_CREDENTIALS"] || process.env["google_application_credentials"] (google-auth-library :14312)`},
	{Name: "GOOGLE_APPLICATION_CREDENTIALS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `first link of the same chain`},
	{Name: "GEMINI_API_KEY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `getEnv("GEMINI_API_KEY") — ambient API key that outranks the config directory; also an untrusted-workspace .env whitelist name`},
	{Name: "GOOGLE_API_KEY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `getEnv("GOOGLE_API_KEY") — second ambient API key`},
	{Name: "GOOGLE_GENAI_USE_VERTEXAI", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GOOGLE_GENAI_USE_VERTEXAI"] === "true" — cloud-mode auth selector`},
	{Name: "GOOGLE_GENAI_USE_GCA", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GOOGLE_GENAI_USE_GCA"] — second cloud-mode selector`},
	// GEMINI_DEFAULT_AUTH_TYPE names the auth type the session uses — a
	// .env could move the session off the account directory onto a named
	// method (login_with_google / use_vertex_ai / …). Selector-class:
	// unconditional pin like GOOGLE_GENAI_USE_VERTEXAI.
	{Name: "GEMINI_DEFAULT_AUTH_TYPE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GEMINI_DEFAULT_AUTH_TYPE"] — auth-type selector (settings.security.auth.selectedType fallback)`},
	// The credential SOURCE selectors and gates: each decides which
	// credential backs the request, not where it goes — keep-if-set so a
	// deliberate operator deployment (compute ADC on GCP, pluggable
	// executables, cloud-shell) still forwards.
	{Name: "GEMINI_CLI_USE_COMPUTE_ADC", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_USE_COMPUTE_ADC"] === "true" — compute-ADC source selector`},
	{Name: "CLOUD_SHELL", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["CLOUD_SHELL"] === "true" — cloud-shell source selector; also triggers setUpCloudShellEnvironment, which rewrites GOOGLE_CLOUD_PROJECT`},
	{Name: "GEMINI_CLI_HOME", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvRoot,
		Read: `process.env["GEMINI_CLI_HOME"] — relocates the CLI's own credential/config directory; the account path injects the account dir here`},
	{Name: "CLOUDSDK_CONFIG", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env.CLOUDSDK_CONFIG || join(APPDATA/HOME, ".config", "gcloud") → getWellKnownCertificateConfigFileLocation — relocates the credential config root google-auth-library reads (chunk-ECNYAST2.js:17420)`},
	{Name: "GCE_METADATA_IP", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `GCE_METADATA_IP || GCE_METADATA_HOST || <compiled 169.254.169.254> — metadata endpoint the ADC probe and token fetch dial (gcp-metadata); the credential source for compute ADC`},
	{Name: "GCE_METADATA_HOST", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `second env link of the metadata chain`},
	{Name: "GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env[GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES] !== "1" — pluggable-auth executable credential gate; "" ≠ "1" holds it closed`},
	{Name: "GEMINI_TELEMETRY_USE_CLI_AUTH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `parseBooleanEnvFlag(env2["GEMINI_TELEMETRY_USE_CLI_AUTH"]) — selects whether the credentialed GCP telemetry client authenticates with the CLI's own credential (chunk-DN4XSYRG.js:248403)`},

	// ── Project selection ──────────────────────────────────────────────
	// Every env link of every chain that names the project a credentialed
	// request carries. google-auth-library's chain is four links
	// (GCLOUD_PROJECT || GOOGLE_CLOUD_PROJECT || gcloud_project ||
	// google_cloud_project); the bundled GCP telemetry exporters resolve
	// OTLP_GOOGLE_CLOUD_PROJECT || GOOGLE_CLOUD_PROJECT with the CLI's
	// credential (#7665 review R2); _GEMINI_USER_GCP_PROJECT overrides the
	// project under vertex-ai in setUpCloudShellEnvironment. All are
	// truthy/|| links — defined-empty is inert — and the account path keeps
	// the operator's forwarded project (keep-if-set).
	{Name: "GCLOUD_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GCLOUD_PROJECT"] || process.env["GOOGLE_CLOUD_PROJECT"] || process.env["gcloud_project"] || process.env["google_cloud_project"] (google-auth-library :14547) — chain HEAD`},
	{Name: "GOOGLE_CLOUD_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `second link of the project chain; also OTLP_GOOGLE_CLOUD_PROJECT || GOOGLE_CLOUD_PROJECT → gcpProjectId for GcpTrace/Log/MetricExporter(projectId, credentials) (chunk-DN4XSYRG.js:251018), and GOOGLE_CLOUD_PROJECT || GOOGLE_CLOUD_PROJECT_ID on the GCA path`},
	{Name: "gcloud_project", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `third (lowercase) link of the project chain`},
	{Name: "google_cloud_project", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `fourth (lowercase) link of the project chain`},
	{Name: "GOOGLE_CLOUD_PROJECT_ID", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GOOGLE_CLOUD_PROJECT"] || process.env["GOOGLE_CLOUD_PROJECT_ID"] — project fallback for the GCA setup path`},
	{Name: "GOOGLE_CLOUD_QUOTA_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GOOGLE_CLOUD_QUOTA_PROJECT"] || null — the X-Goog-User-Project quota override sent WITH the credential`},
	{Name: "OTLP_GOOGLE_CLOUD_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["OTLP_GOOGLE_CLOUD_PROJECT"] || process.env["GOOGLE_CLOUD_PROJECT"] → gcpProjectId for the credentialed Gcp*Exporters — head of the telemetry project chain`},
	{Name: "_GEMINI_USER_GCP_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (!process2.env[USER_GCP_PROJECT]) { save GOOGLE_CLOUD_PROJECT }; under vertex-ai value = process2.env[USER_GCP_PROJECT] → rewrites GOOGLE_CLOUD_PROJECT (chunk-BCYBA52D.js:16327-16346) — a .env pre-set hijacks the project override`},

	// ── Master's bundled-transport carries (#7659) ─────────────────────
	// Claimed before this change; they stay so no table loses a member.
	{Name: "DEBUG_AUTH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env.DEBUG_AUTH) — bundled google-auth-library credential debug logging (#7659)`},
	{Name: "DETECT_GCP_RETRIES", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `DETECT_GCP_RETRIES ? Number(...) : 0 — gcp-metadata retry shaping`},
	{Name: "METADATA_SERVER_DETECTION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env.METADATA_SERVER_DETECTION) — metadata-server detection mode`},

	// ── Endpoint selection (#7655/#7659 carries) ───────────────────────
	// The endpoint the credential rides to — pinned or re-asserted before
	// this change and kept so the tables lose nothing.
	{Name: "GOOGLE_GEMINI_BASE_URL", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `getEnv("GOOGLE_GEMINI_BASE_URL") — gemini endpoint override; truthy also flips getAuthTypeFromEnv to GATEWAY`},
	{Name: "GOOGLE_VERTEX_BASE_URL", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `getEnv("GOOGLE_VERTEX_BASE_URL") — vertex endpoint override`},
	{Name: "CODE_ASSIST_API_VERSION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["CODE_ASSIST_API_VERSION"] || "v1internal" — Code Assist API-version selector on the credential-bearing endpoint`},
	{Name: "CODE_ASSIST_ENDPOINT", Mount: GeminiBundleEnvReassert, Account: GeminiBundleEnvReassert,
		Value: "https://cloudcode-pa.googleapis.com",
		Read:  `process.env["CODE_ASSIST_ENDPOINT"] ?? "https://cloudcode-pa.googleapis.com" — ?? read: defined-empty becomes the endpoint`},
	{Name: "GOOGLE_CLOUD_UNIVERSE_DOMAIN", Mount: GeminiBundleEnvReassert, Account: GeminiBundleEnvReassert,
		Value: "googleapis.com",
		Read:  `... ?? process.env["GOOGLE_CLOUD_UNIVERSE_DOMAIN"] ?? "googleapis.com" → dial host logging.<domain> (bundled @google-cloud/logging / googleapis-common)`},

	// ── Claimed by the shared / allowlist tables ───────────────────────
	// Pre-existing holds owned by the agent-agnostic sets — recorded so the
	// enumeration is complete without moving the claim.
	{Name: "GITHUB_TOKEN", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GITHUB_TOKEN"] — the bundled gh extension's Authorization header; denied+pinned on the mount path by credentialMountSharedCredentialNames, keep-if-set on the account path since #7555`},
	{Name: "HOME", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `credential/config root for ~/.gemini, ~/.config/gcloud; the mount path re-asserts it to the session home`},
	{Name: "PATH", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `program lookup; the mount path re-asserts the image's own PATH`},
	{Name: "SHELL", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `pane command interpreter; the mount path re-asserts /bin/sh`},
	{Name: "XDG_CONFIG_HOME", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `config root (gcloud lives under it); mount re-asserts the session-home default`},
	{Name: "XDG_DATA_HOME", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `env2.XDG_DATA_HOME || join(homedir, ".local", "share") — chrome-devtools-mcp data root; mount re-asserts the session-home default`},
	{Name: "EDITOR", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `editor program; the mount path re-asserts /bin/false`},
	{Name: "VISUAL", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `editor program sibling; the mount path re-asserts /bin/false`},
	{Name: "TMUX", Mount: GeminiBundleEnvNoPin, Account: GeminiBundleEnvKept,
		Read: `tmux server identity the CLI's terminal detection reads; refused to run_args, never pinned — the pane boundary stamps the real value`},
	{Name: "TERM", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `terminal capability`},
	{Name: "COLORTERM", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `"COLORTERM" in env — terminal capability`},
	{Name: "NO_COLOR", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `display toggle`},
	{Name: "FORCE_COLOR", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `"FORCE_COLOR" in env — display toggle`},
	{Name: "LANG", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `env2["LC_ALL"] || env2["LC_CTYPE"] || env2["LANG"] — locale chain link`},
	{Name: "USER", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `identity display`},
	{Name: "DISPLAY", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `if (!process.env["DISPLAY"]) — chrome-devtools-mcp headless check; ambient-admitted`},

	// ── Waived: proxy-chain spellings of the no-credential helper ──────
	// The bundled third_party/index.js proxy helper resolves
	//   npm_config_<proto>_proxy || <proto>_proxy || npm_config_proxy ||
	//   all_proxy   and   npm_config_no_proxy || no_proxy
	// with a getEnv that reads lower||UPPER of every literal — it serves
	// the browser-tools bundle and attaches NO CLI credential, so the
	// npm_config_* spellings stay out of scope while the generic
	// <proto>_proxy / all_proxy links are claimed above.
	{Name: "npm_config_http_proxy", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `getEnv('npm_config_' + proto + '_proxy') — proto=http head of the browser-tools proxy chain`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},
	{Name: "npm_config_https_proxy", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `proto=https spelling of the same head link`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},
	{Name: "npm_config_proxy", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `getEnv('npm_config_proxy') — third link of the same chain`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},
	{Name: "npm_config_no_proxy", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `getEnv('npm_config_no_proxy') || getEnv('no_proxy') — the helper's bypass chain`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},
	{Name: "NPM_CONFIG_HTTP_PROXY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `uppercase case-twin the helper's getEnv reads`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},
	{Name: "NPM_CONFIG_HTTPS_PROXY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `uppercase case-twin the helper's getEnv reads`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},
	{Name: "NPM_CONFIG_PROXY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `uppercase case-twin the helper's getEnv reads`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},
	{Name: "NPM_CONFIG_NO_PROXY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `uppercase case-twin the helper's getEnv reads`,
		Reason: `the bundled npm-config helper attaches no CLI credential`},

	// ── Waived: transport debug / behavior flags ──────────────────────
	// Read by the credential-bearing transports but choosing no proxy,
	// trust root, credential or project.
	{Name: "GRPC_EXPERIMENTAL_ENABLE_OUTLIER_DETECTION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `(process.env.GRPC_EXPERIMENTAL_ENABLE_OUTLIER_DETECTION ?? "true") === "true"`,
		Reason: `client load-balancing flag — selects no proxy/trust/credential/project; a re-assert would also have to be a value judgment over the operator's choice (#7665 R4)`},
	{Name: "GRPC_NODE_USE_ALTERNATIVE_RESOLVER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `(process.env.GRPC_NODE_USE_ALTERNATIVE_RESOLVER ?? "false") === "true"`,
		Reason: `DNS-resolver choice, not proxy/trust/credential/project`},
	{Name: "GRPC_NODE_TRACE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env.GRPC_NODE_TRACE ?? process.env.GRPC_TRACE — trace-channel list`,
		Reason: `debug output selector`},
	{Name: "GRPC_TRACE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `second link of the trace ?? chain`,
		Reason: `debug output selector`},
	{Name: "GRPC_NODE_VERBOSITY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env.GRPC_NODE_VERBOSITY ?? process.env.GRPC_VERBOSITY — log verbosity`,
		Reason: `debug output level`},
	{Name: "GRPC_VERBOSITY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `second link of the verbosity ?? chain`,
		Reason: `debug output level`},
	{Name: "NODE_OPTIONS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["NODE_OPTIONS"] || "" forwarded into spawned node processes`,
		Reason: `generic node runtime flag — not a credential-transport selector`},
	{Name: "NODE_DEBUG", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `node debuglog categories`, Reason: `debug to session stderr`},
	{Name: "HTTP2_DEBUG", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `!!process.env.HTTP2_DEBUG`, Reason: `debug toggle`},

	// ── Waived: credential-adjacent but not a selector ─────────────────
	{Name: "GEMINI_API_KEY_AUTH_MECHANISM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GEMINI_API_KEY_AUTH_MECHANISM"] || "x-goog-api-key" — header the API key rides on`,
		Reason: `chooses the credential's header encoding, not which credential or where it goes`},
	{Name: "GEMINI_CLI_CUSTOM_HEADERS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GEMINI_CLI_CUSTOM_HEADERS"] || void 0 → parseCustomHeaders spread into baseHeaders BEFORE the Authorization assignment (chunk-DN4XSYRG.js:275194-275226)`,
		Reason: `annotates requests; it cannot replace the credential, endpoint or project — and a pin would be a repo-visible behavior change outside the issue's scope`},
	{Name: "GEMINI_FORCE_FILE_STORAGE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env[FORCE_FILE_STORAGE_ENV_VAR] === "true"`,
		Reason: `local credential-storage backend selector — picks where the credential is written, not which one is sent`},
	{Name: "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env[FORCE_ENCRYPTED_FILE_ENV_VAR] === "true"`,
		Reason: `local credential-storage backend selector`},
	{Name: "AWS_ACCESS_KEY_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `AWS workload-federation credential read by bundled google-auth-library`,
		Reason: `operator/cloud deployment credential — ambient config the account path must keep flowing (git/ssh tooling reads it); not the mounted credential`},
	{Name: "AWS_SECRET_ACCESS_KEY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `same federation chain`, Reason: `same`},
	{Name: "AWS_SESSION_TOKEN", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `same federation chain`, Reason: `same`},
	{Name: "AWS_REGION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `AWS region for the federation chain`, Reason: `deployment region, not the mounted credential`},
	{Name: "AWS_DEFAULT_REGION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `AWS_REGION fallback`, Reason: `same`},
	{Name: "GEMINI_CLI_IDE_AUTH_TOKEN", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `connectionConfig?.authToken ?? process.env["GEMINI_CLI_IDE_AUTH_TOKEN"] — bearer for the IDE companion channel`,
		Reason: `the IDE channel's own token, not the mounted credential`},

	// ── Waived: endpoint/UX selectors outside the defended classes ─────
	{Name: "GOOGLE_GENAI_API_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GOOGLE_GENAI_API_VERSION"] — genai apiVersion selector`,
		Reason: `request semantics, not a proxy/trust/credential/project selector`},
	{Name: "GOOGLE_CLOUD_LOCATION", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `getEnv("GOOGLE_CLOUD_LOCATION") — vertex region; admitted through the conditional cloud group (geminiCloudNames) when an operator forwards its selector`},
	{Name: "OAUTH_CALLBACK_HOST", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["OAUTH_CALLBACK_HOST"] || "127.0.0.1"`,
		Reason: `interactive-login loopback binding; the mounted credential flow never opens it`},
	{Name: "OAUTH_CALLBACK_PORT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["OAUTH_CALLBACK_PORT"] parsed`, Reason: `same`},
	{Name: "EDITOR_URL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `typeof process.env["EDITOR_URL"] === "string"`, Reason: `editor service endpoint`},
	{Name: "EDITOR_IN_CLOUD_SHELL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `!!(process.env["EDITOR_IN_CLOUD_SHELL"] || process.env["CLOUD_SHELL"])`, Reason: `editor choice`},
	{Name: "GEMINI_CLI_EXTENSION_REGISTRY_URI", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GEMINI_CLI_EXTENSION_REGISTRY_URI"] ?? (trustedFolder ? settings...) — extension-registry endpoint`,
		Reason: `extension-catalog endpoint; carries no CLI credential`},

	// ── Waived: environment detection → resource labeling ─────────────
	// These read only into is*Environment detection and telemetry
	// resource attributes — they never touch the credential's transport.
	{Name: "CLOUD_RUN_JOB", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env.CLOUD_RUN_JOB || FUNCTION_NAME || K_SERVICE — detection chain`,
		Reason: `GCP-environment detection label`},
	{Name: "FUNCTION_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `chain link`, Reason: `detection label`},
	{Name: "K_SERVICE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `chain link`, Reason: `detection label`},
	{Name: "FUNCTION_TARGET", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `!!process.env[CLOUD_FUNCTION_TARGET_ENV]`, Reason: `detection label`},
	{Name: "K_CONFIGURATION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `!!process.env[CLOUD_RUN_CONFIG_ENV]`, Reason: `detection label`},
	{Name: "GAE_ENV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env[GAE_ENV] === "standard"`, Reason: `detection label`},
	{Name: "GOOGLE_CLOUD_REGION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GOOGLE_CLOUD_REGION || FUNCTION_REGION — region detection chain`, Reason: `detection label`},
	{Name: "FUNCTION_REGION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `second link`, Reason: `detection label`},
	{Name: "KUBERNETES_SERVICE_HOST", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env[KUBERNETES_SERVICE_HOST_ENV] !== void 0 — presence check`,
		Reason: `!== undefined read: a defined-empty pin would itself assert k8s detection; effect is telemetry labeling only`},
	{Name: "GAE_SERVICE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env[GAE_SERVICE_ENV] !== void 0 and || GAE_MODULE_NAME`,
		Reason: `presence check — pinning asserts the detection`},
	{Name: "GAE_MODULE_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `sibling of a presence-checked chain`, Reason: `same`},

	// ── Waived: CI / platform presence detection ───────────────────────
	{Name: "CI", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `"CI" in env and CI === "true" || GITHUB_ACTIONS === "true"`,
		Reason: `CI detection; also a presence read — a defined-empty pin would assert it`},
	{Name: "GITHUB_ACTIONS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `second link + ["GITHUB_ACTIONS", "GITEA_ACTIONS", "CIRCLECI"].some(key in env)`, Reason: `CI detection`},
	{Name: "GITEA_ACTIONS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `presence-scan member`, Reason: `CI detection`},
	{Name: "CIRCLECI", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `presence-scan member`, Reason: `CI detection`},
	{Name: "TRAVIS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `presence-scan member`, Reason: `CI detection`},
	{Name: "APPVEYOR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `presence-scan member`, Reason: `CI detection`},
	{Name: "GITLAB_CI", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `presence-scan member`, Reason: `CI detection`},
	{Name: "BUILDKITE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `presence-scan member`, Reason: `CI detection`},
	{Name: "DRONE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `presence-scan member`, Reason: `CI detection`},
	{Name: "CI_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env.CI_NAME === "codeship"`, Reason: `CI detection`},
	{Name: "TEAMCITY_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `"TEAMCITY_VERSION" in env and env.TEAMCITY_VERSION`, Reason: `CI detection`},
	{Name: "TF_BUILD", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `"TF_BUILD" in env`, Reason: `CI detection`},
	{Name: "AGENT_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `"AGENT_NAME" in env`, Reason: `CI detection`},

	// ── Waived: config roots, trust & workspace ────────────────────────
	{Name: "GEMINI_CLI_TRUST_WORKSPACE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GEMINI_CLI_TRUST_WORKSPACE"] — bypasses the workspace-trust decision`,
		Reason: `gates .env fill breadth; the read runs before the loader, so no .env can claim it — and the ambient/forwarded copies stay deployment config`},
	{Name: "GEMINI_CLI_TRUSTED_FOLDERS_PATH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GEMINI_CLI_TRUSTED_FOLDERS_PATH"] || join(home, ".gemini", "trustedFolders.json")`,
		Reason: `relocates the trust store — file path, not a transport selector`},
	{Name: "GEMINI_CLI_SYSTEM_SETTINGS_PATH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env["GEMINI_CLI_SYSTEM_SETTINGS_PATH"])`, Reason: `settings file location`},
	{Name: "GEMINI_CLI_SYSTEM_DEFAULTS_PATH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env["GEMINI_CLI_SYSTEM_DEFAULTS_PATH"])`, Reason: `defaults file location`},
	{Name: "GEMINI_EXP", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `experiments file`, Reason: `feature-flag source`},
	{Name: "GEMINI_SYSTEM_MD", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GEMINI_SYSTEM_MD path — system prompt file`, Reason: `prompt source, not a transport`},
	{Name: "GEMINI_WRITE_SYSTEM_MD", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["GEMINI_WRITE_SYSTEM_MD"]`, Reason: `prompt output path`},
	{Name: "CHROME_CONFIG_HOME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `puppeteer chrome config root`, Reason: `browser profile root`},
	{Name: "VIRTUAL_ENV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["VIRTUAL_ENV"]?.startsWith(...)`, Reason: `python venv path`},
	{Name: "PYTHONPATH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["PYTHONPATH"] || ""`, Reason: `python search path`},

	// ── Waived: channel / program / process selectors ──────────────────
	{Name: "BROWSER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["BROWSER"] || "open"/"start"/"xdg-open" — OAuth browser program`,
		Reason: `opener program, not a credential transport; an ambient copy is the login pane's tool and must never be pinned`},
	{Name: "NO_BROWSER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `!!process.env["NO_BROWSER"]`, Reason: `auth-flow UX flag — pinning stamps the login pane's own value into sessions`},
	{Name: "GEMINI_CLI_IDE_SERVER_PORT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE channel port`, Reason: `companion-channel config`},
	{Name: "GEMINI_CLI_IDE_WORKSPACE_PATH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE workspace hint`, Reason: `same`},
	{Name: "GEMINI_CLI_IDE_PID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE parent pid`, Reason: `same`},
	{Name: "GEMINI_CLI_IDE_SERVER_STDIO_COMMAND", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE stdio bridge command`, Reason: `same`},
	{Name: "GEMINI_CLI_IDE_SERVER_STDIO_ARGS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE stdio bridge args`, Reason: `same`},
	{Name: "GEMINI_CLI_NO_RELAUNCH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `truthy — skips sandbox relaunch`, Reason: `process control`},
	{Name: "GEMINI_CLI_WORKTREE_HANDLED", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `=== "1" relaunch sentinel`, Reason: `internal sentinel`},
	{Name: "GEMINI_CLI_INTEGRATION_TEST", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `=== "true" test branch`, Reason: `test-only`},
	{Name: "GEMINI_CLI_ACTIVITY_LOG_TARGET", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env["GEMINI_CLI_ACTIVITY_LOG_TARGET"])`, Reason: `log file target`},
	{Name: "GEMINI_DEBUG_LOG_FILE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env["GEMINI_DEBUG_LOG_FILE"])`, Reason: `debug log target`},
	{Name: "GEMINI_CONTEXT_TRACE_DIR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `context-trace dir`, Reason: `debug output dir`},
	{Name: "GEMINI_EVENT_LOOP_MONITOR_ENABLED", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `=== "true" instrumentation toggle`, Reason: `instrumentation`},
	{Name: "DEBUG", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["DEBUG"] === "true" || === "1"`, Reason: `debug mode`},
	{Name: "DEBUG_MODE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `[DEBUG, DEBUG_MODE].some(...)`, Reason: `debug mode`},
	{Name: "DEBUG_PORT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["DEBUG_PORT"] || "9229"`, Reason: `inspector port`},
	{Name: "NODE_ENV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env.NODE_ENV === "test"`, Reason: `node mode flag`},
	{Name: "DEV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["DEV"] === "true"`, Reason: `dev flag`},
	{Name: "GEMINI_MODEL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["GEMINI_MODEL"] — model override`, Reason: `chooses the model, not the credential's destination`},

	// ── Waived: sandbox channel ────────────────────────────────────────
	// Sandbox selection names a process wrapper, never the credential's
	// transport — and re-asserting SEATBELT_PROFILE / GEMINI_SANDBOX_IMAGE
	// would either weaken an operator's forwarded choice or carry a
	// version-specific constant (#7665 R4), so the whole family stays out.
	{Name: "GEMINI_SANDBOX", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["GEMINI_SANDBOX"] — sandbox runtime selector`, Reason: `process wrapper`},
	{Name: "SANDBOX", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["SANDBOX"]`, Reason: `same`},
	{Name: "GEMINI_SANDBOX_IMAGE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GEMINI_SANDBOX_IMAGE"] ?? "us-docker.../sandbox:0.42.0" ?? customImage ?? packageJson`,
		Reason: `a re-assert would pin a version-specific tag over the operator's image — out of scope and out of bounds (#7665 R4)`},
	{Name: "SEATBELT_PROFILE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["SEATBELT_PROFILE"] ??= "permissive-open"`,
		Reason: `a re-assert of permissive-open would weaken a forwarded restrictive profile — never weaken an operator's choice (#7665 R4)`},
	{Name: "GEMINI_SANDBOX_PROXY_COMMAND", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env["GEMINI_SANDBOX_PROXY_COMMAND"])`, Reason: `sandbox program`},
	{Name: "SANDBOX_ENV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env["SANDBOX_ENV"]) split(",")`, Reason: `sandbox env names`},
	{Name: "SANDBOX_FLAGS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shell-quote.parse(..., process.env)`, Reason: `sandbox flags`},
	{Name: "SANDBOX_MOUNTS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env["SANDBOX_MOUNTS"]) split(",")`, Reason: `sandbox mounts`},
	{Name: "SANDBOX_PORTS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `(process.env["SANDBOX_PORTS"] ?? "").split(",")`, Reason: `sandbox ports`},
	{Name: "SANDBOX_SET_UID_GID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process.env["SANDBOX_SET_UID_GID"]?.toLowerCase().trim()`, Reason: `sandbox uid`},
	{Name: "BUILD_SANDBOX", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `sandbox image build toggle`, Reason: `build flag`},

	// ── Waived: telemetry family (alias reads) ─────────────────────────
	// resolveTelemetrySettings reads every one through env2 = process.env
	// (chunk-DN4XSYRG.js:248368-248403). They gate or direct telemetry —
	// the credentialed variant is held by claiming its credential selector
	// (USE_CLI_AUTH) and project chain (OTLP_GOOGLE_CLOUD_PROJECT ||
	// GOOGLE_CLOUD_PROJECT), and the OTLP/collector paths attach no CLI
	// credential at all.
	{Name: "GEMINI_TELEMETRY_ENABLED", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `parseBooleanEnvFlag(env2["GEMINI_TELEMETRY_ENABLED"])`, Reason: `telemetry gate`},
	{Name: "GEMINI_TELEMETRY_TRACES_ENABLED", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `parseBooleanEnvFlag(env2["GEMINI_TELEMETRY_TRACES_ENABLED"])`, Reason: `telemetry gate`},
	{Name: "GEMINI_TELEMETRY_TARGET", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env2["GEMINI_TELEMETRY_TARGET"] — local|gcp parse`, Reason: `telemetry destination gate — the credentialed GCP variant's project chain is claimed`},
	{Name: "GEMINI_TELEMETRY_OTLP_ENDPOINT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env2["GEMINI_TELEMETRY_OTLP_ENDPOINT"] ?? env2["OTEL_EXPORTER_OTLP_ENDPOINT"]`, Reason: `OTLP endpoint — the exporter attaches no CLI credential`},
	{Name: "GEMINI_TELEMETRY_OTLP_PROTOCOL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env2["GEMINI_TELEMETRY_OTLP_PROTOCOL"]`, Reason: `telemetry protocol`},
	{Name: "GEMINI_TELEMETRY_LOG_PROMPTS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `parseBooleanEnvFlag(env2["GEMINI_TELEMETRY_LOG_PROMPTS"])`, Reason: `telemetry content flag — destination is held by the project chain`},
	{Name: "GEMINI_TELEMETRY_OUTFILE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env2["GEMINI_TELEMETRY_OUTFILE"]`, Reason: `telemetry file path`},
	{Name: "GEMINI_TELEMETRY_USE_COLLECTOR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `parseBooleanEnvFlag(env2["GEMINI_TELEMETRY_USE_COLLECTOR"])`, Reason: `collector gate — collector path attaches no CLI credential`},
	// Gated by the USE_GCA selector, which is pinned on both paths.
	{Name: "GOOGLE_CLOUD_ACCESS_TOKEN", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `read only inside the GOOGLE_GENAI_USE_GCA && branch`,
		Reason: `gated by a selector pinned defined-empty on both paths`},

	// ── Waived: OTEL exporter family (per-signal templates) ────────────
	// The vendored OTEL SDK reads OTEL_EXPORTER_OTLP_<SIGNAL>_<PROPERTY>
	// through templates and literal calls; the exporter attaches no CLI
	// credential.
	{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env2["OTEL_EXPORTER_OTLP_ENDPOINT"] — shared OTLP endpoint`, Reason: `telemetry path; no CLI credential`},
	{Name: "OTEL_EXPORTER_OTLP_HEADERS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `OTLP exporter headers`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_INSECURE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: "process.env[`OTEL_EXPORTER_OTLP_INSECURE`]?.toLowerCase().trim()", Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `OTLP shared protocol`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shared CA`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shared client cert`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_CLIENT_KEY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shared client key`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_COMPRESSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shared compression`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TIMEOUT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shared timeout`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_HEADERS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_INSECURE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_HEADERS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_INSECURE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `literal`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `literal`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_HEADERS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_INSECURE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_CLIENT_CERTIFICATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_CLIENT_KEY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `signal-expanded`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_PROMETHEUS_HOST", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `prometheus bind host`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_PROMETHEUS_PORT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `prometheus bind port`, Reason: `telemetry path`},
	{Name: "OTEL_SEMCONV_STABILITY_OPT_IN", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `OTEL semantic-convention opt-in`, Reason: `telemetry schema`},

	// ── Waived: IDE / surface / detection labels ───────────────────────
	{Name: "TERM_PROGRAM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `"TERM_PROGRAM" in env`, Reason: `detection label`},
	{Name: "TERM_PROGRAM_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `terminal-app version`, Reason: `detection label`},
	{Name: "TERM_PRODUCT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `terminal product`, Reason: `detection label`},
	{Name: "TERMINAL_EMULATOR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `?.toLowerCase().includes("jetbrains")`, Reason: `detection label`},
	{Name: "TERMINUS_SUBLIME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `Boolean(env2.TERMINUS_SUBLIME)`, Reason: `detection label`},
	{Name: "ALACRITTY_WINDOW_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env3["ALACRITTY_WINDOW_ID"]`, Reason: `detection label`},
	{Name: "JETBRAINS_IDE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env2["JETBRAINS_IDE"]`, Reason: `detection label`},
	{Name: "MONOSPACE_ENV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE detection`, Reason: `detection label`},
	{Name: "POSITRON", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE detection`, Reason: `detection label`},
	{Name: "ZED_SESSION_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE detection`, Reason: `detection label`},
	{Name: "XCODE_VERSION_ACTUAL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `xcode presence`, Reason: `detection label`},
	{Name: "VSCODE_PID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `vscode detection`, Reason: `detection label`},
	{Name: "VSCODE_GIT_ASKPASS_MAIN", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `vscode askpass detection`, Reason: `detection label`},
	{Name: "VSCODE_GIT_IPC_HANDLE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `vscode git ipc`, Reason: `detection label`},
	{Name: "VSCODE_REMOTE_CONTAINERS_SESSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `vscode remote`, Reason: `detection label`},
	{Name: "WT_SESSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows-terminal detection`, Reason: `detection label`},
	{Name: "STY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `screen detection`, Reason: `detection label`},
	{Name: "SHPOOL_SESSION_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shpool detection`, Reason: `detection label`},
	{Name: "SSH_CONNECTION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `ssh session detection`, Reason: `detection label`},
	{Name: "SSH_CLIENT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `ssh session detection`, Reason: `detection label`},
	{Name: "SSH_TTY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `ssh tty detection`, Reason: `detection label`},
	{Name: "REMOTE_CONTAINERS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `devcontainer detection`, Reason: `detection label`},
	{Name: "CODESPACES", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `codespaces detection`, Reason: `detection label`},
	{Name: "CONTAINER_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `container-name detection`, Reason: `detection label`},
	{Name: "CLOUD_SHELL_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `cloud-shell version`, Reason: `detection label`},
	{Name: "REPLIT_USER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `replit detection`, Reason: `detection label`},
	{Name: "CURSOR_TRACE_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `cursor trace propagation`, Reason: `telemetry label`},
	{Name: "ANTIGRAVITY_CLI_ALIAS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `antigravity surface`, Reason: `detection label`},
	{Name: "GEMINI_CLI_SURFACE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `telemetry surface`, Reason: `telemetry label`},
	{Name: "SURFACE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `telemetry surface`, Reason: `telemetry label`},
	{Name: "CLI_TITLE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `window title`, Reason: `display label`},
	{Name: "HOSTNAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `host label`, Reason: `telemetry label`},
	{Name: "SUDO_UID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `sudo uid`, Reason: `identity display`},
	{Name: "SUDO_GID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `sudo gid`, Reason: `identity display`},
	{Name: "XDG_SESSION_TYPE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `session-type detection`, Reason: `detection label`},
	{Name: "WAYLAND_DISPLAY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `display-server detection`, Reason: `detection label`},
	{Name: "DEBIAN_FRONTEND", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `apt frontend`, Reason: `detection label`},
	{Name: "IS_BINARY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `binary-build detection`, Reason: `detection label`},
	{Name: "TERMUX_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `termux detection`, Reason: `detection label`},
	{Name: "GITHUB_EVENT_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH-actions label`, Reason: `telemetry label`},
	{Name: "GITHUB_REPOSITORY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH-actions label`, Reason: `telemetry label`},
	{Name: "GITHUB_SHA", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH-actions label`, Reason: `telemetry label`},
	{Name: "GH_ISSUE_NUMBER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow label`, Reason: `telemetry label`},
	{Name: "GH_PR_NUMBER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow label`, Reason: `telemetry label`},
	{Name: "GH_WORKFLOW_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow label`, Reason: `telemetry label`},
	{Name: "GH_CUSTOM_TRACKING_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow label`, Reason: `telemetry label`},
	{Name: "GAE_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GAE label`, Reason: `telemetry label`},
	{Name: "GAE_MODULE_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GAE label`, Reason: `telemetry label`},
	{Name: "K_REVISION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `cloud-run label`, Reason: `telemetry label`},

	// ── Waived: platform plumbing ──────────────────────────────────────
	{Name: "USERPROFILE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows home fallback`, Reason: `windows-only`},
	{Name: "APPDATA", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows config root`, Reason: `windows-only`},
	{Name: "AppData", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows spelling`, Reason: `windows-only`},
	{Name: "LOCALAPPDATA", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows config root`, Reason: `windows-only`},
	{Name: "LocalAppData", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows spelling`, Reason: `windows-only`},
	{Name: "COMSPEC", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows shell`, Reason: `windows-only`},
	{Name: "ComSpec", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows spelling`, Reason: `windows-only`},
	{Name: "comspec", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows spelling`, Reason: `windows-only`},
	{Name: "SystemRoot", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows root`, Reason: `windows-only`},
	{Name: "SYSTEMROOT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows root`, Reason: `windows-only`},
	{Name: "WINDIR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows root`, Reason: `windows-only`},
	{Name: "windir", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows spelling`, Reason: `windows-only`},
	{Name: "PATHEXT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows exec extensions`, Reason: `windows-only`},
	{Name: "ProgramFiles", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows install root`, Reason: `windows-only`},
	{Name: "OSTYPE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `platform detection`, Reason: `detection label`},
	{Name: "WSLENV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `WSL env bridge`, Reason: `windows-only`},
	{Name: "WSL_DISTRO_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `WSL detection`, Reason: `detection label`},
	{Name: "WSL_INTEROP", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `WSL detection`, Reason: `detection label`},
	{Name: "VBOX_INSTALL_PATH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `virtualbox path`, Reason: `detection label`},
	{Name: "VBOX_MSI_INSTALL_PATH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `virtualbox path`, Reason: `detection label`},
	{Name: "PREFIX", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `package prefix`, Reason: `install plumbing`},
	{Name: "DESTDIR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `package destdir`, Reason: `install plumbing`},
	{Name: "LANGUAGE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `getEnv("LANGUAGE") — locale`, Reason: `display label`},
	{Name: "LC_ALL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env2["LC_ALL"] || env2["LC_CTYPE"] || env2["LANG"] — locale chain head`, Reason: `display label`},
	{Name: "LC_CTYPE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `second link of the locale chain`, Reason: `display label`},
	{Name: "LC_MESSAGES", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `getEnv("LC_MESSAGES") — locale`, Reason: `display label`},
	{Name: "ZSH_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `getEnv("ZSH_NAME") — shell detection`, Reason: `detection label`},
	{Name: "YARGS_DISABLE_WRAP", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `getEnv("YARGS_DISABLE_WRAP")`, Reason: `display label`},
	{Name: "YARGS_MIN_NODE_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `yargs node floor`, Reason: `tooling metadata`},
	{Name: "_", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `getEnv("_") — self-path marker`, Reason: `display label`},

	// ── Waived: misc / internal ────────────────────────────────────────
	{Name: "LOG_STREAM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env.LOG_STREAM)`, Reason: `yaml debug to session stderr`},
	{Name: "LOG_TOKENS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `yaml token dump`, Reason: `debug to session stderr`},
	{Name: "TEENY_REQUEST_WARN_CONCURRENT_REQUESTS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `concurrency warning threshold`, Reason: `log threshold`},
	{Name: "DOTENV_KEY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `dotenvx vault key`, Reason: `the bundled vault path is dead code — the CLI's own loader reads plain .env`},
	{Name: "GEMINI_CLI_TEST_VAR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `test plumbing`, Reason: `test-only`},
	{Name: "GEMINI_MEMORY_MONITOR_INTERVAL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `monitor interval`, Reason: `instrumentation`},
	{Name: "GEMINI_PTY_INFO", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `pty metadata`, Reason: `instrumentation`},
	{Name: "GRACEFUL_FS_PLATFORM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `graceful-fs shim`, Reason: `implementation internal`},
	{Name: "BUF_BIGINT_DISABLE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `protobuf-es bigint toggle`, Reason: `implementation internal`},
	{Name: "CHOKIDAR_INTERVAL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `fs-watch interval`, Reason: `implementation internal`},
	{Name: "CHOKIDAR_USEPOLLING", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `fs-watch polling`, Reason: `implementation internal`},
	{Name: "QTS_DEBUG", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `querystring debug`, Reason: `implementation internal`},
	{Name: "READABLE_STREAM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `stream impl toggle`, Reason: `implementation internal`},
	{Name: "UNDICI_NO_WASM_SIMD", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `undici wasm toggle`, Reason: `implementation internal`},
	{Name: "WS_NO_BUFFER_UTIL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `ws buffer toggle`, Reason: `implementation internal`},
	{Name: "WS_NO_UTF_8_VALIDATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `ws utf8 toggle`, Reason: `implementation internal`},
	{Name: "TESTING_TAR_FAKE_PLATFORM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `tar test knob`, Reason: `test-only`},
	{Name: "TEST_GRACEFUL_FS_GLOBAL_PATCH", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `graceful-fs test knob`, Reason: `test-only`},
	{Name: "__FAKE_FS_O_FILENAME__", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `memfs test knob`, Reason: `test-only`},
	{Name: "__FAKE_PLATFORM__", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `memfs test knob`, Reason: `test-only`},
	{Name: "__IS_WSL_TEST__", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `wsl test knob`, Reason: `test-only`},
	{Name: "__MINIMATCH_TESTING_PLATFORM__", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `minimatch test knob`, Reason: `test-only`},
	{Name: "JEST_WORKER_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `jest detection`, Reason: `test-only`},
	{Name: "VITEST", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env["VITEST"]`, Reason: `test-only`},
	{Name: "__COG_BASHRC_SOURCED", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env["__COG_BASHRC_SOURCED"] — shell-rc sentinel read in IDE detection`, Reason: `internal sentinel`},
	{Name: "PUPPETEER_TEST_EXPERIMENTAL_CHROME_FEATURES", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `puppeteer test flag`, Reason: `test-only`},
	{Name: "PUPPETEER_WEBDRIVER_BIDI_ONLY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `puppeteer protocol toggle`, Reason: `implementation internal`},
	{Name: "FORCE_GENERIC_KEYBINDING_HINTS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env["FORCE_GENERIC_KEYBINDING_HINTS"] — keybinding hints`, Reason: `display label`},
	{Name: "INK_SCREEN_READER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `env["INK_SCREEN_READER"] — screen-reader mode`, Reason: `display label`},
	{Name: "CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process7.env["CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS"] — usage-stats opt-out`, Reason: `telemetry opt-out`},
	{Name: "CHROME_DEVTOOLS_MCP_CRASH_ON_UNCAUGHT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `process7.env["CHROME_DEVTOOLS_MCP_CRASH_ON_UNCAUGHT"] !== "true"`, Reason: `crash-behavior toggle`},
	{Name: "GEMINI_PROMPT_*", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   "process13.env[`GEMINI_PROMPT_${key.toUpperCase()}`] — prompt-section toggles named by key",
		Reason: `feature toggles, not transports — a prefix family, not a name`},
	{Name: "debug_*", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `Object.keys(process.env).filter(/^debug_/i) — vendored util inspectOpts`,
		Reason: `debug-option family — prefix scan, not a name`},
}

// The classification tables derive from this artifact so they cannot drift
// from the measured list:

// geminiBundleAccountKeepIfSet is the accountLoaderFilledNames["gemini"]
// content: every name whose account-path hold is keep-if-set.
func geminiBundleAccountKeepIfSet() map[string]struct{} {
	out := make(map[string]struct{})
	for _, r := range GeminiBundleEnvReads {
		if r.Account == GeminiBundleEnvKeepIfSet {
			out[r.Name] = struct{}{}
		}
	}
	return out
}

// geminiBundleAccountPins is the accountLateEnvironmentNames["gemini"]
// content: every name pinned defined-and-empty unconditionally.
func geminiBundleAccountPins() map[string]struct{} {
	out := make(map[string]struct{})
	for _, r := range GeminiBundleEnvReads {
		if r.Account == GeminiBundleEnvPin {
			out[r.Name] = struct{}{}
		}
	}
	return out
}

// geminiBundleAccountReasserts is accountLateEnvironmentValues["gemini"]:
// every name re-asserted to a first-party value on the account path.
func geminiBundleAccountReasserts() map[string]string {
	out := make(map[string]string)
	for _, r := range GeminiBundleEnvReads {
		if r.Account == GeminiBundleEnvReassert {
			out[r.Name] = r.Value
		}
	}
	return out
}
