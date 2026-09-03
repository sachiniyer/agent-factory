package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This is the lint half of #3787 part 1, and it exists because the defect it
// guards is a copy-paste defect: seventeen daemon test files independently
// wrote the same four lines.
//
// The four lines point a PROCESS-GLOBAL logger — log.WarningLog / InfoLog /
// ErrorLog — at a local, unsynchronized bytes.Buffer, then read that buffer
// from the test goroutine. Every goroutine alive in the test binary writes into
// whichever sink is currently installed, so the read races any of them:
// `Test (macOS)` went red with `reconcileLateGhostCleanup`, a background
// goroutine from a Manager the failing test never created, writing into that
// test's assertion buffer.
//
// daemon/logcapture_test.go now holds the one synchronized sink and the
// installers for it. This check keeps a new bare buffer from landing beside it.
//
// It is a drift lint, not a sandbox. A test that reached the global logger
// through an intermediate variable in a file of its own would get past rule A;
// rule B is the second layer, and neither is a substitute for reading the diff.
// What it stops is the realistic path — someone copying one of the seventeen
// blocks that used to be here.

// daemonLogCaptureHelper is the ONE daemon test file allowed to install a sink
// on a process-global logger. Everything else goes through the installers it
// exports.
const daemonLogCaptureHelper = "daemon/logcapture_test.go"

var (
	// installViaSetOutput matches `log.WarningLog.SetOutput(`, `aflog.InfoLog.SetOutput(`
	// and friends — the shape all seventeen sites used.
	installViaSetOutput = regexp.MustCompile(`\b(?:Warning|Info|Error)Log\.SetOutput\(`)

	// installViaAssignment matches `log.WarningLog = stdlog.New(&buf, …)`, which
	// daemon/tcpserver_test.go used and which a grep for SetOutput cannot see.
	// It is the worse of the two: replacing the pointer races every goroutine
	// that reads the logger variable, not just the buffer behind it.
	installViaAssignment = regexp.MustCompile(`\b(?:Warning|Info|Error)Log\s*=[^=]`)

	// bareSinkArgument matches a sink handed to SetOutput that is the address of
	// a local, or a bytes.Buffer built inline. Unlike the two above this applies
	// to the helper file as well: the helper must not regress to a bare buffer
	// either.
	bareSinkArgument = regexp.MustCompile(`SetOutput\(\s*(?:&|bytes\.)`)
)

// lintDaemonLogCapture reports problems in daemon test files, keyed
// path -> content. Whole-line `//` comments are stripped first: the call sites
// deliberately explain the fix in prose, and this file's own fixtures below
// would otherwise be unwritable.
func lintDaemonLogCapture(files map[string]string) []string {
	var findings []string
	for name, content := range files {
		helper := filepath.ToSlash(name) == daemonLogCaptureHelper
		for i, line := range strings.Split(content, "\n") {
			at := name + ":" + strconv.Itoa(i+1) + ": "
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if !helper && installViaSetOutput.MatchString(line) {
				findings = append(findings, at+
					"installs a sink on a process-global logger. Those loggers are shared by"+
					" every goroutine in the test binary, so a foreign one writes into your"+
					" buffer while you read it (#3787). Use captureWarnings/captureInfo/"+
					"captureErrors/teeWarnings/silenceWarnings from "+daemonLogCaptureHelper+".")
			}
			if !helper && installViaAssignment.MatchString(line) {
				findings = append(findings, at+
					"replaces a process-global logger variable. That races every goroutine"+
					" reading the variable, on top of the buffer race (#3787). Use the"+
					" installers in "+daemonLogCaptureHelper+".")
			}
			if bareSinkArgument.MatchString(line) {
				findings = append(findings, at+
					"hands SetOutput a bare buffer. bytes.Buffer is unsynchronized, and the"+
					" logger writes into it from whatever goroutine logs (#3787). The sink"+
					" must lock on both Write and String — logCapture does.")
			}
		}
	}
	return findings
}

func TestDaemonLogCaptureLintFlagsTheShapesThatRaced(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		want    bool
	}{
		{
			name:    "the exact four lines this issue removed",
			file:    "daemon/rootagent_singleton_test.go",
			content: "\tvar warnings bytes.Buffer\n\tprevious := log.WarningLog.Writer()\n\tlog.WarningLog.SetOutput(&warnings)\n\tt.Cleanup(func() { log.WarningLog.SetOutput(previous) })\n",
			want:    true,
		},
		{
			name:    "the same shape on InfoLog",
			file:    "daemon/limitresume_test.go",
			content: "\taflog.InfoLog.SetOutput(&buf)\n",
			want:    true,
		},
		{
			name:    "the same shape on ErrorLog",
			file:    "daemon/rootagent_repoprobe_test.go",
			content: "\tlog.ErrorLog.SetOutput(errors)\n",
			want:    true,
		},
		{
			// The shape a SetOutput grep cannot see — tcpserver_test.go's. The
			// issue counted 17 files because of this one.
			name:    "replacing the logger variable outright",
			file:    "daemon/tcpserver_test.go",
			content: "\tlog.WarningLog = stdlog.New(&buf, \"WARNING: \", 0)\n",
			want:    true,
		},
		{
			name:    "reading the logger variable is not replacing it",
			file:    "daemon/tcpserver_test.go",
			content: "\tprev := log.WarningLog\n\tif log.WarningLog == nil {\n\t\treturn\n\t}\n",
			want:    false,
		},
		{
			name:    "the installers",
			file:    "daemon/rootagent_singleton_test.go",
			content: "\twarnings := captureWarnings(t)\n\tinfo := captureInfo(t)\n\tsilenceWarnings(t)\n",
			want:    false,
		},
		{
			name:    "prose about the old shape in a comment is inert",
			file:    "daemon/rootagent_singleton_test.go",
			content: "\t// Was: log.WarningLog.SetOutput(&warnings) — see #3787.\n\twarnings := captureWarnings(t)\n",
			want:    false,
		},
		{
			// The helper is exempt from the two install rules and from nothing
			// else: this is what it actually contains.
			name:    "the helper installing its own synchronized sink",
			file:    daemonLogCaptureHelper,
			content: "\tlogger.SetOutput(capture)\n\taflog.WarningLog.SetOutput(io.MultiWriter(previous, capture))\n\taflog.WarningLog.SetOutput(io.Discard)\n",
			want:    false,
		},
		{
			// …and this is the regression the exemption must not cover.
			name:    "the helper regressing to a bare buffer",
			file:    daemonLogCaptureHelper,
			content: "\taflog.WarningLog.SetOutput(&warnings)\n",
			want:    true,
		},
		{
			name:    "a bytes.Buffer built inline as the sink",
			file:    "daemon/daemon_test.go",
			content: "\tlogger.SetOutput(bytes.NewBuffer(nil))\n",
			want:    true,
		},
		{
			name:    "an unrelated SetOutput on a logger this lint does not own",
			file:    "daemon/daemon_test.go",
			content: "\tcmd.SetOutput(io.Discard)\n",
			want:    false,
		},
		{
			name:    "a struct field of type bytes.Buffer is not a sink",
			file:    daemonLogCaptureHelper,
			content: "type logCapture struct {\n\tmu  sync.Mutex\n\tbuf bytes.Buffer\n}\n",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := lintDaemonLogCapture(map[string]string{tc.file: tc.content})
			if got := len(findings) > 0; got != tc.want {
				t.Fatalf("flagged = %v, want %v (findings: %v)", got, tc.want, findings)
			}
		})
	}
}

// readDaemonTestFiles returns every daemon test file, keyed by repo-relative
// slash path.
func readDaemonTestFiles(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	files := map[string]string{}
	err := filepath.Walk(filepath.Join(root, "daemon"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk daemon: %v", err)
	}
	return files
}

func TestDaemonLogCaptureLintOverTheRealTree(t *testing.T) {
	files := readDaemonTestFiles(t)

	// Anti-vacuity: a scan pointed at the wrong directory would also report
	// nothing. Name files it must have read, including two of the seventeen.
	for _, must := range []string{
		daemonLogCaptureHelper,
		"daemon/daemon_test.go",
		"daemon/rootagent_singleton_test.go",
	} {
		if content, ok := files[must]; !ok || len(content) == 0 {
			t.Fatalf("scan did not read %s — the lint is not looking where it thinks it is", must)
		}
	}

	if findings := lintDaemonLogCapture(files); len(findings) > 0 {
		t.Fatalf("daemon tests install unsynchronized sinks on the global loggers:\n  %s",
			strings.Join(findings, "\n  "))
	}

	// The lint above is also satisfied by having deleted every capture, so the
	// call sites are counted. Twenty-six daemon test files went through this
	// conversion, for 47 installer calls.
	installers := []string{"captureWarnings(t)", "captureInfo(t)", "captureErrors(t)", "teeWarnings(t)", "silenceWarnings(t)"}
	sites := 0
	for _, content := range files {
		for _, installer := range installers {
			sites += strings.Count(content, installer)
		}
	}
	if sites < 40 {
		t.Errorf("found %d capture-installer call sites, want at least the 47 from #3787. "+
			"If a test legitimately stopped asserting on the log, lower this number in the "+
			"same commit and say why", sites)
	}
}

// TestDaemonLogCaptureHelperLocksBothEnds asserts the property the whole change
// rests on, rather than the fact that a helper exists. A logCapture whose
// String() forgot the lock would satisfy every check above and still race
// exactly as #3787 describes — the race report names a Buffer.String() read
// against a Logger.Printf() write, so the READ side is the one that has to
// take the lock.
func TestDaemonLogCaptureHelperLocksBothEnds(t *testing.T) {
	helper := readDaemonTestFiles(t)[daemonLogCaptureHelper]
	if helper == "" {
		t.Fatalf("%s is missing", daemonLogCaptureHelper)
	}
	if !strings.Contains(helper, "sync.Mutex") {
		t.Fatalf("%s declares no mutex", daemonLogCaptureHelper)
	}
	for _, method := range []string{"Write", "String", "Reset"} {
		signature := "func (c *logCapture) " + method
		start := strings.Index(helper, signature)
		if start < 0 {
			t.Fatalf("%s has no %s method", daemonLogCaptureHelper, method)
		}
		body := helper[start:]
		if end := strings.Index(body, "\n}\n"); end >= 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "c.mu.Lock()") {
			t.Errorf("logCapture.%s does not take the lock; a sink locked on only one end "+
				"still races the reader (#3787):\n%s", method, body)
		}
	}
}
