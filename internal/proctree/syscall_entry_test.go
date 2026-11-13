package proctree

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRawSyscalls pins #7241's first item for every platform's files at
// once, which is why it reads source rather than running darwin code.
//
// Every system call proctree makes reads another process's state, and so do
// the reads execgroup's reap makes through it and on its own (kern.proc.pgrp).
// On XNU those reads can sleep uninterruptibly: proc_info's proc_find waits
// for a target that is part-way through an exec, and kern.proc waits the same
// way. A raw syscall never tells the Go scheduler it has entered the kernel,
// so one parked there pins its P, and every stop-the-world waits for it. So
// neither package may name RawSyscall or RawSyscall6, from syscall or
// x/sys/unix, anywhere: not in a call, and not as a value either (#7243), such
// as a `var entry = unix.RawSyscall6` seam a call then goes through.
func TestNoRawSyscalls(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range []string{".", filepath.Join("..", "execgroup")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			checked++
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && strings.HasPrefix(sel.Sel.Name, "RawSyscall") {
					t.Errorf("%s names %s: a read of another process can sleep in the kernel, so enter it as a blocking Syscall",
						fset.Position(sel.Pos()), sel.Sel.Name)
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("found no source files to check")
	}
}
