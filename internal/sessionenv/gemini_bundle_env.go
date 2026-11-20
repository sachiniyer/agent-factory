package sessionenv

// gemini_bundle_env.go is the COMPLETENESS ARTIFACT for the Gemini CLI's
// environment reads (#7662): every variable name the code bundled into
// @google/gemini-cli@GeminiBundleVersion reads from process.env — its own
// source AND the libraries it bundles (@grpc/grpc-js, google-auth-library,
// gcp-metadata, @google-cloud/logging, the npm-config proxy helper) — each
// classified on BOTH defended paths, or waived with the reason it is not
// claimed.
//
// WHY A BUNDLE MEASUREMENT: a source-tree audit of the CLI cannot see the
// bundled libraries' reads — #7655 found googleapis-common's
// GOOGLE_CLOUD_UNIVERSE_DOMAIN, and the #7659 review found grpc-js's proxy
// and trust-root names (R1) plus google-auth-library's lowercase
// GOOGLE_APPLICATION_CREDENTIALS twin and four-link project chain (R2). The
// reads below were measured by `npm pack @google/gemini-cli@0.42.0` into an
// empty directory and grepping the bundle — including process2.env[...]
// aliased reads, getEnv("NAME") helper reads, and process.env[CONST] reads
// whose constant resolves to a name. The CLI was never executed.
//
// THE FILL CHANNEL: gemini's own dotenv loader (loadEnvironment in the
// bundle) walks up from the workspace applying <dir>/.gemini/.env (trusted
// workspaces only) and <dir>/.env fill-if-unset, with ~/.gemini/.env and
// ~/.env fallbacks. Under an UNTRUSTED workspace only AUTH_ENV_VAR_WHITELIST
// fills — GEMINI_API_KEY, GOOGLE_API_KEY, GOOGLE_CLOUD_PROJECT,
// GOOGLE_CLOUD_LOCATION — but trust state is operator state af cannot see at
// the boundary, so every name below is treated as fillable. The defended
// hold is a DEFINED value: the loader assigns only names absent from
// process.env, so defined-and-empty claims a name without giving it a real
// value — provided the read treats empty as inert (a ||, !!, or === check).
// Reads through ?? or !== undefined make defined-empty a REAL value; those
// names are re-asserted to the first-party default instead.
//
// THE ARTIFACT'S JOB: this table is what the classification tables derive
// from (see the helpers at the bottom) and what
// TestGeminiBundleEnvReads_* pin, so a name the bundle gains or drops when
// the CLI version moves is a deliberate, reviewed edit here — the checked
// completeness R3 asked for, replacing the prose completeness claim that
// #7655 and the #7659 review each falsified.
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
	// the operator's claim), an absent one is defined-empty. These are the
	// accountLoaderFilledNames members; on the mount path the same name is
	// denied to run_args and pinned empty unless forwarded.
	GeminiBundleEnvKeepIfSet GeminiBundleEnvClass = "keep-if-set"
	// GeminiBundleEnvReassert re-defines the name with the first-party
	// default Value on both paths: the read treats defined-empty as a real
	// (broken) value — a ?? nullish coalesce — so the claim must carry a
	// working value, not an empty pin.
	GeminiBundleEnvReassert GeminiBundleEnvClass = "reassert"
	// GeminiBundleEnvRoot is the account-scoped credential root af injects
	// (the account's config directory). Never pinned — pinning it empty
	// would erase the selection.
	GeminiBundleEnvRoot GeminiBundleEnvClass = "account-root"
	// GeminiBundleEnvShared is claimed by the SHARED, agent-agnostic
	// boundary tables rather than the artifact-derived sets: the mount
	// path's credentialMountReassertedNames /
	// credentialMountSharedProgramEntries already deny and re-assert these
	// names with values the artifact cannot express as constants (the
	// session home, the image's own PATH, /bin/false). Listing the class
	// here keeps the artifact's claim complete WITHOUT making
	// geminiBundle*Reasserts() pretend it knows a constant value.
	GeminiBundleEnvShared GeminiBundleEnvClass = "shared-boundary"
	// GeminiBundleEnvKept is ambient configuration af deliberately keeps on
	// the recorded path — terminal/locale/display names and the operator's
	// own forwarded claims that need no pin.
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

// GeminiBundleEnvReads is the measured list. Order: the credential-relevant
// reads the boundary exists for, then the claimed-but-not-credential reads,
// then the waived tail.
var GeminiBundleEnvReads = []GeminiBundleEnvRead{
	// ── Proxy selection ────────────────────────────────────────────────
	// grpc-js's getProxyInfo reads grpc_proxy FIRST, then https_proxy,
	// then http_proxy — pinning only the generic spellings leaves the
	// transport-specific head of the chain free (#7659 review R1). The
	// bypass list is no_grpc_proxy || no_proxy. npm-config reads feed the
	// bundled registry fetch helper's own proxy chain. Every one is a
	// truthy/|| read, so defined-empty is inert; the account path keeps a
	// forwarded operator proxy.
	{Name: "grpc_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["grpc_proxy"] || process.env["https_proxy"] || process.env["http_proxy"] (grpc-js mapProxyName, chunk-DN4XSYRG.js:302675)`},
	{Name: "no_grpc_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["no_grpc_proxy"] || process.env["no_proxy"] (grpc-js mapProxyName bypass)`},
	{Name: "npm_config_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `getEnv('npm_config_proxy') || getEnv('http_proxy') || getEnv('https_proxy') (bundled third-party npm-config proxy helper)`},
	{Name: "npm_config_no_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `getEnv('npm_config_no_proxy') || getEnv('no_proxy') (same helper)`},
	{Name: "all_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `getEnv('all_proxy') (same helper); the ALL_PROXY spelling is its case twin — denied on the mount path via the shared routing set`},
	{Name: "HTTP_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `second link of the grpc-js proxy chain (as https_proxy sibling) and the generic proxy name every client reads`},
	{Name: "HTTPS_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["https_proxy"] — generic proxy read; grpc-js chain middle link`},
	{Name: "NO_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["no_proxy"] bypass list — second link of no_grpc_proxy || no_proxy`},
	{Name: "http_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["http_proxy"] — grpc-js chain tail link`},
	{Name: "https_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["https_proxy"] — grpc-js chain middle link (lowercase spelling wins for clients that prefer it)`},
	{Name: "no_proxy", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["no_proxy"] — bypass list, tail link of no_grpc_proxy || no_proxy`},
	{Name: "ALL_PROXY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `case twin of the measured getEnv('all_proxy') read — not itself read by the bundle; classified with its twin per #7662`},

	// ── Trust roots / TLS transport ────────────────────────────────────
	{Name: "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `var DEFAULT_ROOTS_FILE_PATH = process.env.GRPC_DEFAULT_SSL_ROOTS_FILE_PATH; if (DEFAULT_ROOTS_FILE_PATH) { readFileSync } → createSecureContext ca (grpc-js tls-helpers, chunk-7VVHSNDQ.js:19800) — empty is falsy, falls back to compiled roots`},
	{Name: "GRPC_SSL_CIPHER_SUITES", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `exports.CIPHER_SUITES = process.env.GRPC_SSL_CIPHER_SUITES → createSecureContext ciphers (same file); measured: ciphers:"" is accepted and the context builds — empty is inert`},
	{Name: "GOOGLE_API_CERTIFICATE_CONFIG", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (envPath = process.env[CERTIFICATE_CONFIGURATION_ENV_VARIABLE]) isValidFile(envPath) → mTLS client-cert config (google-auth-library); empty is falsy, falls through to the gcloud well-known path`},
	{Name: "CLOUDSDK_CONFIG", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env.CLOUDSDK_CONFIG || join(home, ".config", "gcloud") — relocates the gcloud config root the bundled transports read (application-default credentials, cert-config well-known path)`},
	{Name: "GRPC_EXPERIMENTAL_ENABLE_OUTLIER_DETECTION", Mount: GeminiBundleEnvReassert, Account: GeminiBundleEnvReassert,
		Value: "true",
		Read:  `(process.env.GRPC_EXPERIMENTAL_ENABLE_OUTLIER_DETECTION ?? "true") === "true" — a ?? read: defined-empty becomes "false" and disables the default outlier detection`},
	{Name: "GRPC_NODE_USE_ALTERNATIVE_RESOLVER", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `(process.env.GRPC_NODE_USE_ALTERNATIVE_RESOLVER ?? "false") === "true" — defined-empty compares "false", inert`},
	{Name: "GRPC_NODE_TRACE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `grpc trace-channel list — debug output for the credential-bearing transport`},
	{Name: "GRPC_TRACE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `alias read of GRPC_NODE_TRACE`},
	{Name: "GRPC_NODE_VERBOSITY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `grpc log verbosity — debug output level`},
	{Name: "GRPC_VERBOSITY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `alias read of GRPC_NODE_VERBOSITY`},
	{Name: "NODE_OPTIONS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["NODE_OPTIONS"] || "" forwarded into spawned node/sandbox processes`},

	// ── Identity / auth source ─────────────────────────────────────────
	// google_application_credentials is the LOWERCASE twin behind the
	// pinned uppercase name: google-auth-library reads
	// GOOGLE_APPLICATION_CREDENTIALS || google_application_credentials, so
	// the uppercase pin alone left the chain's second link free (#7659 R2).
	// It is an unconditional identity pin like its twin — and admitted
	// through the same conditional group so an operator's Vertex session
	// still forwards it.
	{Name: "google_application_credentials", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GOOGLE_APPLICATION_CREDENTIALS"] || process.env["google_application_credentials"] (google-auth-library :14312)`},
	{Name: "GOOGLE_APPLICATION_CREDENTIALS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `first link of the same chain`},
	{Name: "GEMINI_API_KEY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `getEnv("GEMINI_API_KEY") — ambient API key that outranks the config directory; also in the untrusted-workspace .env whitelist`},
	{Name: "GOOGLE_API_KEY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `getEnv("GOOGLE_API_KEY") — same; second untrusted-whitelist name`},
	{Name: "GOOGLE_GENAI_USE_VERTEXAI", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GOOGLE_GENAI_USE_VERTEXAI"] === "true" — cloud-mode auth selector`},
	{Name: "GOOGLE_GENAI_USE_GCA", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GOOGLE_GENAI_USE_GCA"] — second cloud-mode selector; also gates GOOGLE_CLOUD_ACCESS_TOKEN`},
	// GEMINI_DEFAULT_AUTH_TYPE names the auth type the session uses — a
	// .env could move the session off the account directory onto a named
	// method (login_with_google / use_vertex_ai / ...). Selector-class:
	// unconditional pin like GOOGLE_GENAI_USE_VERTEXAI.
	{Name: "GEMINI_DEFAULT_AUTH_TYPE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvPin,
		Read: `process.env["GEMINI_DEFAULT_AUTH_TYPE"] — auth-type selector (settings.security.auth.selectedType fallback)`},
	{Name: "GEMINI_API_KEY_AUTH_MECHANISM", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_API_KEY_AUTH_MECHANISM"] || "x-goog-api-key" — chooses the header the API key rides on`},
	{Name: "GEMINI_FORCE_FILE_STORAGE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env[FORCE_FILE_STORAGE_ENV_VAR] === "true" — credential-storage mode selector`},
	{Name: "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env[FORCE_ENCRYPTED_FILE_ENV_VAR] === "true" — credential-storage mode selector`},
	{Name: "GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env[GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES] !== "1" — pluggable-auth executable gate; "" ≠ "1" keeps executables refused`},
	// GITHUB_TOKEN is the gh extension's Authorization header. It is
	// common-admitted ambient config on the account path (operator's gh
	// token must survive for git operations), keep-if-set so a .env cannot
	// claim it when the operator has none; the mount path denies+pins it
	// through the shared credential set.
	{Name: "GITHUB_TOKEN", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GITHUB_TOKEN"] — gh/octokit Authorization header for the bundled extension client`},
	{Name: "AWS_ACCESS_KEY_ID", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `AWS workload-federation credential read by bundled google-auth-library`},
	{Name: "AWS_SECRET_ACCESS_KEY", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `same federation chain`},
	{Name: "AWS_SESSION_TOKEN", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `same federation chain`},
	{Name: "AWS_REGION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `AWS region for the federation chain`},
	{Name: "AWS_DEFAULT_REGION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `AWS_REGION fallback link`},
	{Name: "GEMINI_CLI_IDE_AUTH_TOKEN", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_IDE_AUTH_TOKEN"] ?? "" — bearer token for the IDE companion-channel handshake; empty only disables the token check af's own channel never needs`},

	// ── Endpoints ──────────────────────────────────────────────────────
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
	{Name: "GCE_METADATA_IP", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `GCE_METADATA_IP || GCE_METADATA_HOST || <compiled 169.254.169.254> — metadata endpoint the ADC probe and token fetch dial (gcp-metadata)`},
	{Name: "GCE_METADATA_HOST", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `second env link of the same chain`},
	{Name: "GOOGLE_GENAI_API_VERSION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GOOGLE_GENAI_API_VERSION"] — genai apiVersion selector on the credential-bearing request path`},
	{Name: "OAUTH_CALLBACK_HOST", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["OAUTH_CALLBACK_HOST"] || "127.0.0.1" — host the OAuth loopback callback binds`},
	{Name: "OAUTH_CALLBACK_PORT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["OAUTH_CALLBACK_PORT"] parsed → callback listener port`},
	{Name: "EDITOR_URL", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `typeof process.env["EDITOR_URL"] === "string" → editor service endpoint`},
	{Name: "EDITOR_IN_CLOUD_SHELL", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `!!(process.env["EDITOR_IN_CLOUD_SHELL"] || process.env["CLOUD_SHELL"]) → editor choice`},
	{Name: "GEMINI_CLI_EXTENSION_REGISTRY_URI", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_EXTENSION_REGISTRY_URI"] ?? (trustedFolder ? settings.experimental?.extensionRegistryURI : void 0) — extension-registry endpoint; "" is falsy at the use site and behaves like unset`},

	// ── Project selection ──────────────────────────────────────────────
	// The project-id chain is four links: GCLOUD_PROJECT ||
	// GOOGLE_CLOUD_PROJECT || gcloud_project || google_cloud_project —
	// holding only GOOGLE_CLOUD_PROJECT leaves both flanks free (#7659 R2).
	// All are truthy links, so defined-empty is inert; the account path
	// keeps the operator's forwarded project (keep-if-set), and the whole
	// family sits in the conditional group so a Vertex-mode operator's
	// values reach the CLI.
	{Name: "GCLOUD_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GCLOUD_PROJECT"] || process.env["GOOGLE_CLOUD_PROJECT"] || process.env["gcloud_project"] || process.env["google_cloud_project"] (google-auth-library :14547) — chain HEAD`},
	{Name: "GOOGLE_CLOUD_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `second link of the project chain; also process.env["GOOGLE_CLOUD_PROJECT"] || process.env["GOOGLE_CLOUD_PROJECT_ID"] for the GCA path, and an untrusted-whitelist .env name`},
	{Name: "gcloud_project", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `third (lowercase) link of the project chain`},
	{Name: "google_cloud_project", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `fourth (lowercase) link of the project chain`},
	{Name: "GOOGLE_CLOUD_PROJECT_ID", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GOOGLE_CLOUD_PROJECT"] || process.env["GOOGLE_CLOUD_PROJECT_ID"] — project fallback for the GCA setup path`},
	{Name: "GOOGLE_CLOUD_QUOTA_PROJECT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GOOGLE_CLOUD_QUOTA_PROJECT"] || null — the X-Goog-User-Project quota override sent WITH the credential`},
	{Name: "GOOGLE_CLOUD_LOCATION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `getEnv("GOOGLE_CLOUD_LOCATION") — vertex region; fourth untrusted-whitelist .env name`},

	// ── Auth-source and environment detection ─────────────────────────
	{Name: "CLOUD_SHELL", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["CLOUD_SHELL"] === "true" — getAuthTypeFromEnv selector AND setUpCloudShellEnvironment trigger (can overwrite GOOGLE_CLOUD_PROJECT)`},
	{Name: "GEMINI_CLI_USE_COMPUTE_ADC", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_USE_COMPUTE_ADC"] === "true" — second auth-source selector`},
	{Name: "DEBUG_AUTH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env.DEBUG_AUTH) — bundled google-auth-library credential debug logging (#7659)`},
	{Name: "DETECT_GCP_RETRIES", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `DETECT_GCP_RETRIES ? Number(...) : 0 — gcp-metadata retry shaping`},
	{Name: "METADATA_SERVER_DETECTION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env.METADATA_SERVER_DETECTION) — metadata-server detection mode`},
	// Cloud-environment detection: each is a truthy read feeding
	// isGFEnvironment / resource detection, which picks auth sources and
	// telemetry labels. Empty is falsy on every one → inert pins.
	{Name: "CLOUD_RUN_JOB", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env.CLOUD_RUN_JOB || FUNCTION_NAME || K_SERVICE — GCP-environment detection chain`},
	{Name: "FUNCTION_NAME", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `chain link + function-name telemetry label`},
	{Name: "K_SERVICE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `chain link + service-name telemetry label`},
	{Name: "FUNCTION_TARGET", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `!!process.env[CLOUD_FUNCTION_TARGET_ENV] — functions detection`},
	{Name: "K_CONFIGURATION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `!!process.env[CLOUD_RUN_CONFIG_ENV] — cloud-run detection`},
	{Name: "GAE_ENV", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env[GAE_ENV] === "standard" — GAE-standard detection; "" ≠ "standard" → inert`},
	{Name: "GOOGLE_CLOUD_REGION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `GOOGLE_CLOUD_REGION || FUNCTION_REGION || region — region label + detection fallback`},
	{Name: "FUNCTION_REGION", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `second env link of the region chain`},
	// Presence-check detection: these three are read `!== undefined`, so a
	// defined-empty pin would ASSERT the detection — no defined value is
	// inert. Their only bundle effect is GCP-resource telemetry labeling,
	// so they are waived rather than claimed.
	{Name: "KUBERNETES_SERVICE_HOST", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env[KUBERNETES_SERVICE_HOST_ENV] !== void 0 — presence check`,
		Reason: `!== undefined read: a defined-empty pin would itself assert k8s detection; its only bundle effect is telemetry resource labeling`},
	{Name: "GAE_SERVICE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env[GAE_SERVICE_ENV] !== void 0 and || GAE_MODULE_NAME — presence + truthy reads`,
		Reason: `!== undefined read: defined-empty still asserts App-Engine detection; effect is telemetry labeling only`},
	{Name: "GAE_MODULE_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["GAE_MODULE_NAME"] — the || twin of GAE_SERVICE`,
		Reason: `sibling of a presence-checked chain: pinning it alone cannot hold the detection, and its effect is telemetry labeling`},

	// ── Config / trust roots and workspace trust ──────────────────────
	{Name: "GEMINI_CLI_HOME", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvRoot,
		Read: `process.env["GEMINI_CLI_HOME"] — relocates the CLI's own credential/config directory; the account path injects the account dir here`},
	{Name: "GEMINI_CLI_TRUST_WORKSPACE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_TRUST_WORKSPACE"] — bypasses the workspace-trust decision, which gates .env fill breadth`},
	{Name: "GEMINI_CLI_TRUSTED_FOLDERS_PATH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_TRUSTED_FOLDERS_PATH"] || join(home, ".gemini", "trustedFolders.json") — relocates the trust store`},
	{Name: "GEMINI_CLI_SYSTEM_SETTINGS_PATH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["GEMINI_CLI_SYSTEM_SETTINGS_PATH"]) — relocates the system settings file`},
	{Name: "GEMINI_CLI_SYSTEM_DEFAULTS_PATH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["GEMINI_CLI_SYSTEM_DEFAULTS_PATH"]) — relocates the system-defaults file`},
	{Name: "GEMINI_EXP", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `experiments file the CLI reads for feature flags`},
	{Name: "GEMINI_SYSTEM_MD", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `GEMINI_SYSTEM_MD path — file the CLI reads as the system prompt`},
	{Name: "GEMINI_WRITE_SYSTEM_MD", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_WRITE_SYSTEM_MD"] — writes the resolved system prompt to a file`},
	{Name: "CHROME_CONFIG_HOME", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `puppeteer's chrome config root — relocates the browser profile root the CLI's browser tools use`},
	{Name: "VIRTUAL_ENV", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["VIRTUAL_ENV"]?.startsWith(...) — venv path used to construct sandbox environment paths`},
	{Name: "PYTHONPATH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["PYTHONPATH"] || "" — python search path for spawned tools`},

	// ── Channel / program / process selectors ─────────────────────────
	// The OAuth-flow UX pair is deliberately UNCLAIMED on both paths: an
	// account-scoped session must not carry the login pane's own stamps
	// (TestAccountLoginEnvironment_NeverReachesAnAccountScopedSession), and
	// neither name chooses a transport carrying the credential — a session
	// runs repository code already, so the opener program adds no channel.
	{Name: "BROWSER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["BROWSER"] || "open"/"start"/"xdg-open" — program the OAuth flow spawns`,
		Reason: `URL-opener program selection, not a credential-transport read; an ambient copy is also the login pane's tool and must never be pinned into a working session`},
	{Name: "NO_BROWSER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `!!process.env["NO_BROWSER"] — suppresses the OAuth browser open`,
		Reason: `auth-flow UX flag, not a credential-transport read; pinning it would stamp the login pane's own NO_BROWSER=true into every account session`},
	{Name: "GEMINI_CLI_IDE_SERVER_PORT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `IDE companion channel port (pairs with GEMINI_CLI_IDE_AUTH_TOKEN)`},
	{Name: "GEMINI_CLI_IDE_WORKSPACE_PATH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `IDE channel workspace hint`},
	{Name: "GEMINI_CLI_IDE_PID", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `IDE channel parent pid`},
	{Name: "GEMINI_CLI_IDE_SERVER_STDIO_COMMAND", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `IDE channel stdio bridge command (project-.env excluded by default, .gemini/.env still fills)`},
	{Name: "GEMINI_CLI_IDE_SERVER_STDIO_ARGS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `IDE channel stdio bridge args (same exclusion)`},
	{Name: "GEMINI_CLI_NO_RELAUNCH", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `truthy — skips the CLI's sandbox relaunch`},
	{Name: "GEMINI_CLI_WORKTREE_HANDLED", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_WORKTREE_HANDLED"] === "1" — relaunch sentinel the CLI sets on itself`},
	{Name: "GEMINI_CLI_INTEGRATION_TEST", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_INTEGRATION_TEST"] === "true" — flips test-only branches`},
	{Name: "GEMINI_CLI_CUSTOM_HEADERS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_CLI_CUSTOM_HEADERS"] — JSON headers injected into EVERY credential-bearing request`},
	{Name: "GEMINI_CLI_ACTIVITY_LOG_TARGET", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["GEMINI_CLI_ACTIVITY_LOG_TARGET"]) — file the activity log is written to`},
	{Name: "GEMINI_DEBUG_LOG_FILE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["GEMINI_DEBUG_LOG_FILE"]) — debug log file target`},
	{Name: "GEMINI_CONTEXT_TRACE_DIR", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `context-trace output directory`},
	{Name: "GEMINI_EVENT_LOOP_MONITOR_ENABLED", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_EVENT_LOOP_MONITOR_ENABLED"] === "true" — instrumentation toggle`},
	{Name: "DEBUG", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["DEBUG"] === "true" || === "1" — debug mode (project-.env excluded by default; .gemini/.env still fills)`},
	{Name: "DEBUG_MODE", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `[process.env["DEBUG"], process.env["DEBUG_MODE"]].some(...) — debug-mode fallback read`},
	{Name: "DEBUG_PORT", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["DEBUG_PORT"] || "9229" — inspector port for sandboxed debug`},
	{Name: "NODE_ENV", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env.NODE_ENV === "test" — node mode flag read by bundled deps`},
	{Name: "DEV", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["DEV"] === "true" — dev-mode flag`},
	{Name: "GITHUB_ACTIONS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `CI === "true" || GITHUB_ACTIONS === "true" — CI detection`},
	{Name: "CI", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `first link of the same CI check`},
	{Name: "NODE_DEBUG", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `node debuglog categories → session stderr`},
	{Name: "HTTP2_DEBUG", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `!!process.env.HTTP2_DEBUG — node http2 debug`},
	{Name: "GEMINI_MODEL", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_MODEL"] — model override; chooses which model the credential is spent on`},

	// ── Sandbox channel ───────────────────────────────────────────────
	{Name: "GEMINI_SANDBOX", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["GEMINI_SANDBOX"] — sandbox runtime selector (docker/podman/sandbox-exec command)`},
	{Name: "SANDBOX", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["SANDBOX"] — sandbox runtime selector sibling`},
	{Name: "GEMINI_SANDBOX_PROXY_COMMAND", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["GEMINI_SANDBOX_PROXY_COMMAND"]) — program the sandbox proxy execs`},
	{Name: "SANDBOX_ENV", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["SANDBOX_ENV"]) split(",") — names copied into the sandbox env`},
	{Name: "SANDBOX_FLAGS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["SANDBOX_FLAGS"]) shell-quote.parse(..., process.env) — extra flags interpolated from the environment`},
	{Name: "SANDBOX_MOUNTS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `if (process.env["SANDBOX_MOUNTS"]) split(",") — extra bind mounts`},
	{Name: "SANDBOX_PORTS", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `(process.env["SANDBOX_PORTS"] ?? "").split(",") — published ports`},
	{Name: "SANDBOX_SET_UID_GID", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `process.env["SANDBOX_SET_UID_GID"]?.toLowerCase().trim() — "1"/"true" vs "0"/"false" vs platform fallback; "" is neither, inert`},
	{Name: "BUILD_SANDBOX", Mount: GeminiBundleEnvPin, Account: GeminiBundleEnvKeepIfSet,
		Read: `sandbox image build toggle`},
	{Name: "GEMINI_SANDBOX_IMAGE", Mount: GeminiBundleEnvReassert, Account: GeminiBundleEnvReassert,
		Value: "us-docker.pkg.dev/gemini-code-dev/gemini-cli/sandbox:0.42.0",
		Read:  `process.env["GEMINI_SANDBOX_IMAGE"] ?? "us-docker.pkg.dev/gemini-code-dev/gemini-cli/sandbox:0.42.0" ?? customImage ?? packageJson?.config?.sandboxImageUri — ?? read: defined-empty is a real image name`},
	{Name: "SEATBELT_PROFILE", Mount: GeminiBundleEnvReassert, Account: GeminiBundleEnvReassert,
		Value: "permissive-open",
		Read:  `process.env["SEATBELT_PROFILE"] ??= "permissive-open" — ??= read: defined-empty stays and fails builtin-profile membership`},

	// ── Shared/base environment already classified ────────────────────
	// These reads land in the shared, agent-agnostic boundary tables — the
	// mount path re-asserts HOME/PATH/SHELL/the XDG roots with values only
	// the provisioner knows (credentialMountReassertedNames), and the
	// editor pair takes /bin/false through credentialMountSharedProgramEntries.
	{Name: "HOME", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `credential/config root for ~/.gemini, ~/.config/gcloud; the mount path re-asserts it to the session home`},
	{Name: "PATH", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `program lookup; the mount path re-asserts the image's own PATH`},
	{Name: "SHELL", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `pane command interpreter; the mount path re-asserts /bin/sh`},
	{Name: "XDG_CONFIG_HOME", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `config root (gcloud lives under it); the mount path re-asserts the session-home default`},
	{Name: "TMUX", Mount: GeminiBundleEnvNoPin, Account: GeminiBundleEnvKept,
		Read: `tmux server identity the CLI's terminal detection reads; refused to run_args, never pinned — the pane boundary stamps the real value`},
	{Name: "TERM", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `terminal capability`},
	{Name: "COLORTERM", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `terminal capability`},
	{Name: "NO_COLOR", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `display toggle`},
	{Name: "FORCE_COLOR", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `display toggle`},
	{Name: "LANG", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `locale`},
	{Name: "USER", Mount: GeminiBundleEnvKept, Account: GeminiBundleEnvKept,
		Read: `identity display`},
	{Name: "EDITOR", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `editor program; the mount path re-asserts /bin/false`},
	{Name: "VISUAL", Mount: GeminiBundleEnvShared, Account: GeminiBundleEnvKept,
		Read: `editor program sibling; the mount path re-asserts /bin/false`},

	// ── Waived: telemetry / detection labels ──────────────────────────
	// Gated by the USE_GCA selector, which is pinned on both paths — an
	// ambient token still cannot turn the mode on (#7659 review R5).
	{Name: "GOOGLE_CLOUD_ACCESS_TOKEN", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `read only inside the GOOGLE_GENAI_USE_GCA && branch`,
		Reason: `gated by a selector that is pinned defined-empty on both paths; no read reaches it`},
	// OTLP/telemetry endpoints redirect telemetry content only — the OTLP
	// exporter carries no CLI credential (#7659 review R5).
	{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read:   `process.env["OTEL_EXPORTER_OTLP_ENDPOINT"]?.trim() — OTLP telemetry endpoint`,
		Reason: `telemetry path; attaches no CLI credential`},
	{Name: "OTEL_EXPORTER_OTLP_HEADERS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `OTLP exporter headers`, Reason: `telemetry path; attaches no CLI credential`},
	{Name: "OTEL_EXPORTER_OTLP_INSECURE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `OTLP insecure toggle`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_PROMETHEUS_HOST", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `prometheus exporter bind host`, Reason: `telemetry path`},
	{Name: "OTEL_EXPORTER_PROMETHEUS_PORT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `prometheus exporter bind port`, Reason: `telemetry path`},
	{Name: "OTEL_SEMCONV_STABILITY_OPT_IN", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `OTEL semantic-convention opt-in`, Reason: `telemetry schema flag`},
	{Name: "OTLP_GOOGLE_CLOUD_PROJECT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `OTLP project label`, Reason: `telemetry path; attaches no CLI credential`},
	// Environment/IDE/surface detection and telemetry labels — none routes
	// or substitutes a credential.
	{Name: "TERM_PROGRAM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `terminal-app detection`, Reason: `display/detection label`},
	{Name: "TERM_PROGRAM_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `terminal-app version`, Reason: `display/detection label`},
	{Name: "TERM_PRODUCT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `terminal product label`, Reason: `display/detection label`},
	{Name: "TERMINAL_EMULATOR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `terminal detection`, Reason: `display/detection label`},
	{Name: "MONOSPACE_ENV", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `IDE-environment detection`, Reason: `detection label`},
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
		Read: `vscode git ipc detection`, Reason: `detection label`},
	{Name: "VSCODE_REMOTE_CONTAINERS_SESSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `vscode remote detection`, Reason: `detection label`},
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
		Read: `cloud-shell version detection`, Reason: `detection label`},
	{Name: "REPLIT_USER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `replit detection`, Reason: `detection label`},
	{Name: "CURSOR_TRACE_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `cursor trace propagation`, Reason: `telemetry label`},
	{Name: "ANTIGRAVITY_CLI_ALIAS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `antigravity surface label`, Reason: `detection label`},
	{Name: "GEMINI_CLI_SURFACE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `telemetry surface label`, Reason: `telemetry label`},
	{Name: "SURFACE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `telemetry surface label`, Reason: `telemetry label`},
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
	{Name: "TRAVIS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `CI detection`, Reason: `detection label`},
	{Name: "DEBIAN_FRONTEND", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `apt frontend detection`, Reason: `detection label`},
	{Name: "IS_BINARY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `binary-build detection`, Reason: `detection label`},
	{Name: "TERMUX_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `termux platform detection`, Reason: `detection label`},
	{Name: "GITHUB_EVENT_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH-actions telemetry label`, Reason: `telemetry label`},
	{Name: "GITHUB_REPOSITORY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH-actions telemetry label`, Reason: `telemetry label`},
	{Name: "GITHUB_SHA", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH-actions telemetry label`, Reason: `telemetry label`},
	{Name: "GH_ISSUE_NUMBER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow telemetry`, Reason: `telemetry label`},
	{Name: "GH_PR_NUMBER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow telemetry`, Reason: `telemetry label`},
	{Name: "GH_WORKFLOW_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow telemetry`, Reason: `telemetry label`},
	{Name: "GH_CUSTOM_TRACKING_ID", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GH workflow telemetry`, Reason: `telemetry label`},
	{Name: "GAE_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GAE version label`, Reason: `telemetry label`},
	{Name: "GAE_MODULE_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `GAE module version label`, Reason: `telemetry label`},
	{Name: "K_REVISION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `cloud-run revision label`, Reason: `telemetry label`},
	// Platform plumbing — Windows roots and package helpers unreachable on
	// the defended transports (linux/darwin images), or purely internal.
	{Name: "USERPROFILE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows home fallback`, Reason: `windows-only root; defended transports are linux/darwin`},
	{Name: "APPDATA", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows config root`, Reason: `windows-only`},
	{Name: "AppData", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows config root spelling`, Reason: `windows-only`},
	{Name: "LOCALAPPDATA", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows config root`, Reason: `windows-only`},
	{Name: "LocalAppData", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows config root spelling`, Reason: `windows-only`},
	{Name: "COMSPEC", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows shell`, Reason: `windows-only`},
	{Name: "ComSpec", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows shell spelling`, Reason: `windows-only`},
	{Name: "comspec", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows shell spelling`, Reason: `windows-only`},
	{Name: "SystemRoot", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows root`, Reason: `windows-only`},
	{Name: "SYSTEMROOT", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows root`, Reason: `windows-only`},
	{Name: "WINDIR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows root`, Reason: `windows-only`},
	{Name: "windir", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `windows root spelling`, Reason: `windows-only`},
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
		Read: `package-manager prefix`, Reason: `install plumbing`},
	{Name: "DESTDIR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `package-manager destdir`, Reason: `install plumbing`},
	{Name: "LANGUAGE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `locale fallback`, Reason: `display label`},
	{Name: "LC_ALL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `locale`, Reason: `display label`},
	{Name: "LC_MESSAGES", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `locale`, Reason: `display label`},
	{Name: "ZSH_NAME", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shell detection`, Reason: `detection label`},
	{Name: "YARGS_DISABLE_WRAP", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `help-text wrap`, Reason: `display label`},
	{Name: "YARGS_MIN_NODE_VERSION", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `yargs node floor`, Reason: `tooling metadata`},
	{Name: "_", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `self-path marker`, Reason: `display label`},
	// Logging/debug internals — write to the session's own stderr or a
	// file the CLI writes, not a credential channel.
	{Name: "LOG_STREAM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `if (process.env.LOG_STREAM) — yaml lexer debug`, Reason: `debug to session stderr`},
	{Name: "LOG_TOKENS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `yaml token dump`, Reason: `debug to session stderr`},
	{Name: "TEENY_REQUEST_WARN_CONCURRENT_REQUESTS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `concurrency warning threshold`, Reason: `log-warning threshold`},
	{Name: "DOTENV_KEY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `dotenvx vault key`, Reason: `the bundled dotenvx vault path is dead code — the CLI's own loader reads plain .env files`},
	// Test/implementation internals.
	{Name: "GEMINI_CLI_TEST_VAR", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `test plumbing`, Reason: `test-only`},
	{Name: "GEMINI_MEMORY_MONITOR_INTERVAL", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `monitor interval`, Reason: `instrumentation`},
	{Name: "GEMINI_PTY_INFO", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `pty metadata`, Reason: `instrumentation`},
	{Name: "GRACEFUL_FS_PLATFORM", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `graceful-fs platform shim`, Reason: `implementation internal`},
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
		Read: `ws buffer-util toggle`, Reason: `implementation internal`},
	{Name: "WS_NO_UTF_8_VALIDATE", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `ws utf8-validate toggle`, Reason: `implementation internal`},
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
		Read: `vitest detection`, Reason: `test-only`},
	{Name: "__COG_BASHRC_SOURCED", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `shell-rc sentinel`, Reason: `internal sentinel`},
	{Name: "PUPPETEER_TEST_EXPERIMENTAL_CHROME_FEATURES", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `puppeteer test flag`, Reason: `test-only`},
	{Name: "PUPPETEER_WEBDRIVER_BIDI_ONLY", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `puppeteer protocol toggle`, Reason: `implementation internal`},
	{Name: "FORCE_GENERIC_KEYBINDING_HINTS", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `keybinding hint display`, Reason: `display label`},
	{Name: "INK_SCREEN_READER", Mount: GeminiBundleEnvWaived, Account: GeminiBundleEnvWaived,
		Read: `screen-reader mode`, Reason: `display label`},
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
