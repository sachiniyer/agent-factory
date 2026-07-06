package app

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

type clipboardCommandSpec struct {
	path string
	args []string
}

var copyToClipboard = copyTextToClipboard

func copyTextToClipboard(text string) error {
	spec, err := clipboardCommandForPlatform(runtime.GOOS, exec.LookPath)
	if err != nil {
		return err
	}
	return runClipboardCommand(exec.Command(spec.path, spec.args...), text)
}

func clipboardCommandForPlatform(goos string, lookPath func(string) (string, error)) (clipboardCommandSpec, error) {
	switch goos {
	case "darwin":
		path, err := lookPath("pbcopy")
		if err != nil {
			return clipboardCommandSpec{}, noClipboardToolErr()
		}
		return clipboardCommandSpec{path: path}, nil
	default:
		if path, err := lookPath("wl-copy"); err == nil {
			return clipboardCommandSpec{path: path}, nil
		}
		if path, err := lookPath("xclip"); err == nil {
			return clipboardCommandSpec{path: path, args: []string{"-selection", "clipboard"}}, nil
		}
		return clipboardCommandSpec{}, noClipboardToolErr()
	}
}

func runClipboardCommand(copyCmd *exec.Cmd, text string) error {
	var stderr bytes.Buffer
	copyCmd.Stdin = strings.NewReader(text)
	copyCmd.Stderr = &stderr

	if err := copyCmd.Run(); err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return fmt.Errorf("copy failed: %s", stderrText)
		}
		return fmt.Errorf("copy failed: %w", err)
	}
	return nil
}

func noClipboardToolErr() error {
	return errors.New("no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS)")
}
