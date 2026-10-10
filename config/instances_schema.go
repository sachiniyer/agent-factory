package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/log"
)

var errInstancesSchemaContent = errors.New("invalid instances.json schema content")

type instancesEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Instances     json.RawMessage `json:"instances"`
}

// NewInstancesSchemaMigrationPlan returns the v0 array-root -> v1 envelope
// migration plan for a single per-repo instances.json file.
func NewInstancesSchemaMigrationPlan(path string) SchemaMigrationPlan {
	registry := NewSchemaMigrationRegistry()
	if err := registry.Register(LegacySchemaVersion, migrateLegacyInstancesArray); err != nil {
		panic(err)
	}
	return SchemaMigrationPlan{
		StoreName:      InstancesFileName,
		Path:           path,
		CurrentVersion: InstancesSchemaVersion,
		DetectVersion:  detectInstancesSchemaVersion,
		Migrators:      registry,
		Validate:       validateInstancesEnvelope,
		Perm:           0644,
		// detectInstancesSchemaVersion is DetectJSONSchemaVersion plus a
		// "null" -> legacy pre-case, and the probe refuses "null" (it is not a
		// JSON object), so the two agree wherever the probe answers true.
		ProveCurrentVersion: ProveJSONSchemaVersion,
		// instances.json is af-managed state at a path af chose, so the
		// migration write-back keeps the plain writer's semantics — the same
		// side of #3672 every other write to this file is on. Stated rather
		// than left to the zero value because the store next door
		// (task/schema_migration.go) answers the other way, and a reader
		// comparing the two should find the decision, not its absence.
		LinkPolicy: SchemaWriteReplaceLink,
	}
}

// MigrateRepoInstancesForDaemonLoad upgrades one repo's instances.json in
// place. It is intended for daemon-owned load paths; read-only callers should
// use LoadRepoInstances, which tolerates both array-root and envelope formats
// without writing.
func MigrateRepoInstancesForDaemonLoad(repoID string) (SchemaMigrationResult, error) {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return SchemaMigrationResult{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SchemaMigrationResult{}, nil
		}
		return SchemaMigrationResult{}, fmt.Errorf("failed to read repo instances: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return SchemaMigrationResult{}, nil
	}
	_, result, err := LoadAndMigrateSchemaFile(NewInstancesSchemaMigrationPlan(path))
	return result, err
}

// repoInstanceIDsForDaemonLoad lists the repo-scoped subdirectories under the
// instances directory. It is the single walk shared by the daemon-load migrator
// (MigrateAllRepoInstancesForDaemonLoad) and the upgrade manifest
// (RepoInstancesMigrateOnLoadPaths) so the two can never enumerate a different
// set. A missing instances directory is not an error: a daemon with no per-repo
// state has nothing to migrate.
//
// A subdirectory whose name is not a valid repoID is skipped here, the same way
// a non-directory entry already is. repoInstancesPath validates the id before
// touching the filesystem, so feeding an unvalidated name to either consumer
// fails at that pre-condition — a failure that is neither corrupted content nor
// a write failure and so aborts the whole sweep under both callers. Skipping at
// the walk keeps that class from blocking daemon startup while preserving the
// hard-refuse posture for real repos with read/write/newer-schema problems. This
// matches LoadAllRepoInstancesReportingMissing, which already logs and
// continues on the identical case (state.go).
func repoInstanceIDsForDaemonLoad() ([]string, error) {
	dir, err := instancesDirPath()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read instances directory: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := ValidateRepoID(entry.Name()); err != nil {
			log.WarningLog.Printf("skipping instances subdirectory %q: not a valid repo id: %v", entry.Name(), err)
			continue
		}
		ids = append(ids, entry.Name())
	}
	return ids, nil
}

// MigrateAllRepoInstancesForDaemonLoad upgrades every readable per-repo
// instances.json before daemon restore/refresh reads it. Corrupted legacy files
// keep the existing skip-and-warn behavior; newer schema versions and write
// failures are returned so the daemon never overwrites a file it cannot safely
// understand or migrate.
func MigrateAllRepoInstancesForDaemonLoad() error {
	ids, err := repoInstanceIDsForDaemonLoad()
	if err != nil {
		return err
	}
	for _, repoID := range ids {
		if _, err := MigrateRepoInstancesForDaemonLoad(repoID); err != nil {
			var newer *UnsupportedSchemaVersionError
			switch {
			case errors.As(err, &newer):
				return fmt.Errorf("failed to migrate instances for repo %s: %w", repoID, err)
			case errors.Is(err, errInstancesSchemaContent):
				// Preserve the daemon's existing corruption posture: a malformed
				// repo file is skipped and named, not allowed to abort every other
				// repo's restore. The later LoadAllRepoInstances path will log the
				// same repo if it still cannot decode.
				continue
			default:
				return fmt.Errorf("failed to migrate instances for repo %s: %w", repoID, err)
			}
		}
	}
	return nil
}

// RepoInstancesMigrateOnLoadPaths returns the absolute path of every per-repo
// instances.json that MigrateAllRepoInstancesForDaemonLoad rewrites in place at
// daemon load. It walks the SAME repo set as that migrator, so the upgrade
// transaction manifest (#2212 R3) snapshots exactly the files a candidate would
// migrate on boot — letting a binary-only rollback to the previous daemon restore
// state in a schema it can still read, instead of stranding it on a vN+1 file it
// cannot parse. Errors propagate rather than skip: an incomplete manifest silently
// under-protects the rollback, which is the failure mode this whole path exists to
// prevent.
func RepoInstancesMigrateOnLoadPaths() ([]string, error) {
	ids, err := repoInstanceIDsForDaemonLoad()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(ids))
	for _, repoID := range ids {
		path, err := repoInstancesPath(repoID)
		if err != nil {
			return nil, fmt.Errorf("resolve instances path for repo %s: %w", repoID, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// extractInstancesArray migrates raw to the current schema and returns its
// instances array, decoding the envelope EXACTLY ONCE (#3726).
//
// The decode is not cheap and it used to happen twice per read. Migration
// validates the bytes it is about to hand back — for this store that means
// decoding the envelope and normalizing the array — and this function then threw
// that away and decoded the same bytes again for the array it wanted.
// `json.RawMessage` copies what it captures, so each pass copied the whole
// instances array (1.36 MB on this repo's largest file) and normalization
// unmarshalled it into a []json.RawMessage and marshalled it straight back.
//
// So take the validator's work instead of repeating it: swap in a Validate that
// keeps the array it decoded. The plan is a value, so this affects no other
// caller, and validation still runs INSIDE MigrateSchemaBytes — which is what
// keeps every error identically worded and identically classified. The store's
// default validator is the same function with the array discarded, so the two
// cannot drift.
func extractInstancesArray(raw []byte, path string) (json.RawMessage, error) {
	var instances json.RawMessage
	validated := false
	plan := NewInstancesSchemaMigrationPlan(path)
	plan.Validate = func(migrated []byte) error {
		validated = true
		var err error
		instances, err = decodeInstancesEnvelope(migrated)
		return err
	}
	if _, _, err := MigrateSchemaBytes(raw, plan); err != nil {
		return nil, err
	}
	// MigrateSchemaBytes calls Validate on every path that returns nil, so this
	// cannot fire today. It is here because the alternative failure mode is
	// silent: a future path that skipped validation would hand callers a nil
	// array and no error, which reads as "this repo has no sessions" — the
	// clobbering bug #766 exists to prevent.
	if !validated {
		return nil, fmt.Errorf("%w: instances envelope was migrated without being decoded", errInstancesSchemaContent)
	}
	return instances, nil
}

// loadRepoInstancesForAll reads one repo's instances.json for the all-repo
// loaders. missing reports that the file did not exist, which the returned "[]" alone
// cannot say: every all-repo reader treats a missing file as an empty repo, but
// the daemon must not count one as a successful re-read of a repo it skipped
// (#4783).
func loadRepoInstancesForAll(repoID string) (raw json.RawMessage, missing bool, err error) {
	path, err := repoInstancesPath(repoID)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return json.RawMessage("[]"), true, nil
		}
		return nil, false, fmt.Errorf("failed to read repo instances: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return json.RawMessage("[]"), false, nil
	}
	instances, err := extractInstancesArray(data, path)
	if err == nil {
		return instances, false, nil
	}
	var newer *UnsupportedSchemaVersionError
	if errors.As(err, &newer) {
		return nil, false, err
	}
	// All-repo callers historically decoded each repo's raw bytes themselves
	// so they could aggregate and name corrupted repos (#730). Preserve that
	// behavior even though single-repo reads now unwrap envelopes here.
	return json.RawMessage(data), false, nil
}

func marshalInstancesEnvelope(data json.RawMessage) ([]byte, error) {
	instances, err := normalizeJSONRawArray(data, "instances")
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(instancesEnvelope{
		SchemaVersion: InstancesSchemaVersion,
		Instances:     instances,
	}, "", "  ")
}

func migrateLegacyInstancesArray(raw []byte) ([]byte, error) {
	return marshalInstancesEnvelope(raw)
}

// decodeInstancesEnvelopeStruct decodes one instances.json envelope and
// validates its schema version, returning the decoded envelope with Instances
// still a json.RawMessage — the array is decoded only far enough to bound it,
// not normalized. It is the shared validation both the verbatim fast path and
// the normalizing slow path run, so the two cannot drift on what they accept
// or what error they name.
func decodeInstancesEnvelopeStruct(raw []byte) (instancesEnvelope, error) {
	var envelope instancesEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return instancesEnvelope{}, fmt.Errorf("%w: failed to parse instances envelope: %v", errInstancesSchemaContent, err)
	}
	if envelope.SchemaVersion != InstancesSchemaVersion {
		return instancesEnvelope{}, fmt.Errorf("schema_version = %d, want %d", envelope.SchemaVersion, InstancesSchemaVersion)
	}
	return envelope, nil
}

// decodeInstancesEnvelope decodes one instances.json envelope, checks it, and
// returns its normalized instances array. It is the single decode both readers
// share: validateInstancesEnvelope is this with the array dropped, and
// extractInstancesArray is this with the array kept (#3726).
func decodeInstancesEnvelope(raw []byte) (json.RawMessage, error) {
	envelope, err := decodeInstancesEnvelopeStruct(raw)
	if err != nil {
		return nil, err
	}
	return normalizeJSONRawArray(envelope.Instances, "instances")
}

func validateInstancesEnvelope(raw []byte) error {
	_, err := decodeInstancesEnvelope(raw)
	return err
}

// proveInstancesEnvelopeSchemaVersion decodes only the schema_version of one
// instances.json envelope — without materializing its instances member — so the
// verbatim fast path can reject a type-mismatched duplicate schema_version the
// map-based probe lets through without paying the array copy a full envelope
// decode would (#5237: a ~4.7 MB instances.json is unmarshalled twice on every
// cache miss — daemon startup, and after any file update — when this only needs
// the version field).
//
// The decode uses the same json struct-field assignment the slow path's
// decodeInstancesEnvelopeStruct does: last-wins with a first-type-error on a
// duplicate schema_version, so a file the struct decoder rejects here is one it
// rejects in the slow path too. It decodes into a struct that carries only
// schema_version, so the (possibly multi-megabyte) instances array is never
// materialized. The error text differs from decodeInstancesEnvelopeStruct's by
// the probe struct's name alone; the classification (errInstancesSchemaContent
// for a parse error) matches, which is what the read path branches on.
func proveInstancesEnvelopeSchemaVersion(raw []byte) error {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("%w: failed to parse instances envelope: %v", errInstancesSchemaContent, err)
	}
	if probe.SchemaVersion != InstancesSchemaVersion {
		return fmt.Errorf("schema_version = %d, want %d", probe.SchemaVersion, InstancesSchemaVersion)
	}
	return nil
}

// instancesArrayInCurrentEnvelope returns the instances member of raw verbatim
// — no re-marshal of the array — for bytes ProveJSONSchemaVersion has already
// proved are a well-formed current-version document under the map detector's
// rules (#5169). The array is sliced out, not normalized: json.Valid ran inside
// the probe, so every member value this pulls out is a complete JSON value and
// every '['-led one is a complete array — already the normalized array in the
// only sense normalizeJSONRawArray's callers use it, since the array goes
// straight to an unmarshal that whitespace cannot change.
//
// ProveJSONSchemaVersion's contract is to match DetectJSONSchemaVersion, a
// map[string]any decode where duplicate keys collapse last-wins with no type
// error. The struct decoder this envelope goes through elsewhere
// (decodeInstancesEnvelope) is stricter: it assigns each schema_version in
// order into an int field and errors on the FIRST one that cannot go there. A
// file whose LAST schema_version is a valid integer but an EARLIER one is a
// string, bool, object, array, or float proves current to the probe yet is
// corrupt to the struct decoder — and a verbatim return trusts the probe. So
// validate the schema_version here with the same struct-field assignment
// semantics but without decoding the instances array, refusing what the rest of
// the pipeline refuses while the array stays un-materialized, restoring the
// parity the pre-#5169 fast path held by running plan.Validate (the struct
// decode) even when the probe proved current.
//
// Only a plainly array-shaped member takes the verbatim fast return. Everything
// else — an absent member, one found under a case-variant key, null, or a
// non-array value — defers to the same decodeInstancesEnvelope the slow path
// ran, so the verdict and the error text cannot drift: the decoder's
// case-insensitive field match and its nil-RawMessage handling of an explicit
// null reproduce exactly what the migration-validated read produced.
func instancesArrayInCurrentEnvelope(raw []byte) (json.RawMessage, error) {
	if member, found := lastTopLevelJSONMember(raw, "instances"); found {
		if trimmed := bytes.TrimSpace(member); len(trimmed) > 0 && trimmed[0] == '[' {
			if err := proveInstancesEnvelopeSchemaVersion(raw); err != nil {
				return nil, err
			}
			return member, nil
		}
	}
	return decodeInstancesEnvelope(raw)
}

func detectInstancesSchemaVersion(raw []byte) (int, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return LegacySchemaVersion, nil
	}
	version, err := DetectJSONSchemaVersion(raw)
	if err != nil {
		return LegacySchemaVersion, fmt.Errorf("%w: %v", errInstancesSchemaContent, err)
	}
	return version, nil
}

func normalizeJSONRawArray(raw json.RawMessage, field string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: %s must be a JSON array", errInstancesSchemaContent, field)
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("[]"), nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("%w: %s must be a JSON array: %v", errInstancesSchemaContent, field, err)
	}
	if items == nil {
		return json.RawMessage("[]"), nil
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s array: %w", field, err)
	}
	return json.RawMessage(out), nil
}
