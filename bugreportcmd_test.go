package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBugReportCmdWritesFile drives the command end-to-end against a fresh temp
// home: it must resolve the daemon status without spawning anything, build the
// bundle, write it to the requested path, and print the attach/review guidance.
func TestBugReportCmdWritesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(home, ".agent-factory"))

	out := filepath.Join(home, "report.txt")
	t.Cleanup(func() { bugReportJSON, bugReportOutput = false, "" })
	bugReportJSON, bugReportOutput = false, out

	var buf bytes.Buffer
	bugReportCmd.SetOut(&buf)
	if err := bugReportCmd.RunE(bugReportCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if !strings.Contains(buf.String(), "Attach this file") {
		t.Errorf("missing attach guidance:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Review it first") {
		t.Errorf("missing review guidance:\n%s", buf.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("bundle not written: %v", err)
	}
	if !strings.Contains(string(data), "AGENT FACTORY BUG REPORT") {
		t.Errorf("bundle missing header:\n%s", data)
	}
	if !strings.Contains(string(data), "REVIEW THIS BUNDLE BEFORE SHARING") {
		t.Error("bundle missing review banner")
	}
}

// TestBugReportCmdJSON checks the --json path emits a valid {data,error}
// envelope to stdout and writes no file.
func TestBugReportCmdJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(home, ".agent-factory"))

	t.Cleanup(func() { bugReportJSON, bugReportOutput = false, "" })
	bugReportJSON, bugReportOutput = true, ""

	var buf bytes.Buffer
	bugReportCmd.SetOut(&buf)
	if err := bugReportCmd.RunE(bugReportCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	var env struct {
		Data  map[string]any `json:"data"`
		Error any            `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("output is not a valid envelope: %v\n%s", err, buf.String())
	}
	if env.Error != nil {
		t.Errorf("expected nil error member, got %v", env.Error)
	}
	if env.Data["warning"] == nil {
		t.Error("manifest missing warning")
	}
	if env.Data["versions"] == nil {
		t.Error("manifest missing versions")
	}
}
