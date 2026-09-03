package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #3672: the files af MANAGES in its own home refuse a symlinked destination
// rather than replacing it (today's behaviour before this change) or writing
// through it. One test per writer, because the decision was taken per caller and
// a shared helper test would not notice a caller that never adopted it.
//
// Each asserts the same three things, which together are the whole promise:
// the error names BOTH ends, the link is still a link, and the file it points at
// still holds what it held. The last one is what makes this different from a
// plain "it errored" test — a refusal that had already clobbered the target
// would satisfy the first two.

// assertRefusedSymlink is the shared assertion. It takes the error the writer
// returned plus the two paths involved.
func assertRefusedSymlink(t *testing.T, err error, link, target, targetContent string) {
	t.Helper()
	require.Error(t, err, "an af-managed file must refuse a symlinked destination")
	assert.ErrorIs(t, err, config.ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link, "the error names the link")
	assert.Contains(t, err.Error(), target, "and the target it points at")

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the link is left exactly as the user made it")

	got, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, targetContent, string(got),
		"and nothing was written through it")
}

// linkedManagedFile makes <dir>/<name> a symlink to a file elsewhere holding
// content, and returns both paths.
func linkedManagedFile(t *testing.T, dir, name, content string) (link, target string) {
	t.Helper()
	target = filepath.Join(t.TempDir(), "target-"+name)
	require.NoError(t, os.WriteFile(target, []byte(content), 0600))
	link = filepath.Join(dir, name)
	require.NoError(t, os.Symlink(target, link))
	return link, target
}

// The bearer token is the sharpest case: its 0600 mode IS the local auth model,
// so following a link would apply af's secret and af's mode to a file somebody
// else chose, and replacing one would discard their arrangement while
// `af token rotate` kept writing past it.
func TestWriteTokenRefusesASymlinkedTokenFile(t *testing.T) {
	link, target := linkedManagedFile(t, t.TempDir(), daemonTokenFileName, "someone-elses-secret\n")

	assertRefusedSymlink(t, writeToken(link, "af-minted-token"), link, target, "someone-elses-secret\n")
}

// The editor-origin secret mirrors the token deliberately (same 0700 directory,
// same lock, same 0600 write), so it must mirror the refusal too — a second
// hand-rolled answer here is how the two drift.
func TestEnsureEditorOriginSecretRefusesASymlinkedSecretFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	link, target := linkedManagedFile(t, home, editorOriginSecretFileName, "")
	// A dangling-looking EMPTY target is the interesting shape here:
	// loadEditorOriginSecret treats an empty file as absent, so the loader falls
	// through to minting and reaches the writer, which is the path under test.
	_, err := ensureEditorOriginSecret()

	assertRefusedSymlink(t, err, link, target, "")
}

// The VS Code owner record is af-generated process proof — written, re-read to
// authorize a signal, then deleted. A user has no reason to link it, so a link
// is a surprise af should report rather than resolve either way.
func TestWriteVSCodeOwnerRefusesASymlinkedOwnerRecord(t *testing.T) {
	link, target := linkedManagedFile(t, t.TempDir(), "abcdef01-12345678"+vscodeOwnerExt, "{}\n")

	err := writeVSCodeOwner(link, vscodeOwnerRecord{Key: "k", InstanceID: "i", PID: 1})

	assertRefusedSymlink(t, err, link, target, "{}\n")
}

// The event-queue cursor is a byte offset into the jsonl file beside it. The two
// are created, advanced and dropped together, so a cursor that lived in another
// directory would put compaction's cross-file fsync fence on the wrong one.
func TestPersistCursorRefusesASymlinkedCursorFile(t *testing.T) {
	dir := t.TempDir()
	q := newEventQueue(dir, "task-1")
	link, target := linkedManagedFile(t, dir, "task-1.cursor", "999\n")

	q.mu.Lock()
	err := q.persistCursorValueLocked(42)
	q.mu.Unlock()

	assertRefusedSymlink(t, err, link, target, "999\n")
}

// The unit file is what #3672 is titled after. ~/.config/systemd/user/ is a
// directory where links are ordinary — that is how `systemctl enable` works — so
// this is the one place a link is genuinely likely.
func TestInstallAutostartRefusesASymlinkedUnitFile(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	calls := stubAutostartUnitCommand(t, nil)
	stubAutostartStopDaemon(t, true, nil)

	link, target := linkedManagedFile(t, dir, autostartUnitName, "[Unit]\nDescription=not af\n")

	_, err := InstallAutostart()

	assertRefusedSymlink(t, err, link, target, "[Unit]\nDescription=not af\n")
	assert.Empty(t, *calls,
		"and it fails before enabling anything, so no unit is enabled against a file af did not write")
}

// The cleanup half, and the actual asymmetry in the issue title: a failed
// install used to os.Remove the path it wrote. With the write refusing a link,
// af can never have written through one — so unlinking one here would delete an
// arrangement af never touched.
func TestRemoveAutostartUnitFileRefusesToUnlinkALink(t *testing.T) {
	dir := t.TempDir()
	link, target := linkedManagedFile(t, dir, autostartUnitName, "[Unit]\nDescription=not af\n")

	removeAutostartUnitFile(link)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"a failed install must not unlink a link it did not write through")
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "[Unit]\nDescription=not af\n", string(content))
}

// Uninstall is the other remove, and it SURFACES the refusal rather than logging
// it: `af daemon autostart uninstall` reporting success while the link survives
// would be the worst of both.
func TestUninstallAutostartRefusesASymlinkedUnitFile(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	stubAutostartUnitCommand(t, nil)

	link, target := linkedManagedFile(t, dir, autostartUnitName, "[Unit]\nDescription=not af\n")

	_, err := UninstallAutostart()

	assertRefusedSymlink(t, err, link, target, "[Unit]\nDescription=not af\n")
}
