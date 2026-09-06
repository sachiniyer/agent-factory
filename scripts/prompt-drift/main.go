// prompt-drift compares a task prompt without running any of its instructions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func comparePrompt(id, file string, readTask func() ([]byte, error)) error {
	expected, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read prompt file: %w", err)
	}
	raw, err := readTask()
	if err != nil {
		return fmt.Errorf("read task: %w", err)
	}
	var response struct {
		Data *struct {
			ID     string  `json:"id"`
			Prompt *string `json:"prompt"`
		} `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode task: %w", err)
	}
	if len(response.Error) > 0 && string(response.Error) != "null" {
		return fmt.Errorf("task read returned an error: %s", response.Error)
	}
	if response.Data == nil || response.Data.ID != id || response.Data.Prompt == nil {
		return fmt.Errorf("task read did not return prompt for %s", id)
	}
	if !bytes.Equal(expected, []byte(*response.Data.Prompt)) {
		return fmt.Errorf("live prompt differs from %s (byte comparison)", file)
	}
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: prompt-drift <task-id> <prompt-file>")
		os.Exit(2)
	}
	id, file := os.Args[1], os.Args[2]
	err := comparePrompt(id, file, func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "af", "tasks", "get", id, "--json").Output()
	})
	if err != nil {
		fmt.Printf("FINDING: task %s prompt drift check: %v. Report only; do not update the task.\n", id, err)
		os.Exit(1)
	}
}
