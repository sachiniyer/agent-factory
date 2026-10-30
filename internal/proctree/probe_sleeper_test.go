package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

var (
	probeSleeperOnce sync.Once
	probeSleeperPath string
)

// probeSleeper builds a standalone sleep: a real ELF that does not dispatch on argv[0].
func probeSleeper(t testing.TB) (string, error) {
	probeSleeperOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "probesleep")
		src := filepath.Join(dir, "main.go")
		_ = os.WriteFile(src, []byte("package main\nimport (\"os\";\"strconv\";\"time\")\nfunc main(){n,_:=strconv.ParseFloat(os.Args[1],64);time.Sleep(time.Duration(n*float64(time.Second)))}\n"), 0o644)
		out := filepath.Join(dir, "sleeper")
		if b, err := exec.Command("go", "build", "-o", out, src).CombinedOutput(); err != nil {
			panic(string(b))
		}
		probeSleeperPath = out
	})
	return probeSleeperPath, nil
}

var _ = strconv.Itoa
var _ = time.Second
