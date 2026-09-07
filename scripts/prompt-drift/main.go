// prompt-drift compares a task prompt without running any of its instructions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func runCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		diagnostic := strings.TrimSpace(stderr.String())
		if name == "af" {
			var envelope struct {
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal(stderr.Bytes(), &envelope) == nil && len(envelope.Error) > 0 && string(envelope.Error) != "null" {
				diagnostic = fmt.Sprintf("task read returned an error: %s; stderr: %s", envelope.Error, diagnostic)
			}
		}
		return nil, fmt.Errorf("%s %s: %w; stderr: %s", name, strings.Join(args, " "), err, diagnostic)
	}
	return stdout.Bytes(), nil
}

func comparePrompt(id, file string) (bool, error) {
	if _, err := runCommand("git", "fetch", "origin", "master"); err != nil {
		return false, err
	}
	expected, err := runCommand("git", "show", "origin/master:"+file)
	if err != nil {
		return false, err
	}
	raw, err := runCommand("af", "tasks", "get", id, "--json")
	if err != nil {
		return false, fmt.Errorf("read task: %w", err)
	}
	var response struct {
		Data *struct {
			ID     string  `json:"id"`
			Prompt *string `json:"prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return false, fmt.Errorf("decode task: %w", err)
	}
	if response.Data == nil || response.Data.ID != id || response.Data.Prompt == nil {
		return false, fmt.Errorf("task read did not return prompt for %s", id)
	}
	return !bytes.Equal(expected, []byte(*response.Data.Prompt)), nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Println("TOOLING: could not establish master's version (usage: prompt-drift <task-id> <repo-relative-prompt-file>)")
		os.Exit(2)
	}
	id, file := os.Args[1], os.Args[2]
	drift, err := comparePrompt(id, file)
	if err != nil {
		fmt.Printf("TOOLING: task %s prompt drift check: could not establish master's version (%v). Report only; do not update the task.\n", id, err)
		os.Exit(2)
	}
	if drift {
		fmt.Printf("FINDING: task %s live prompt differs from origin/master:%s (byte comparison). Report only; do not update the task.\n", id, file)
		os.Exit(1)
	}
}
