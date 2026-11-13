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
// Every system call this package makes reads another process's state, and on
// XNU those reads can sleep uninterruptibly: proc_info's proc_find waits for a
// target that is part-way through an exec, and kern.proc waits the same way.
// A raw syscall never tells the Go scheduler it has entered the kernel, so one
// parked there pins its P, and every stop-the-world waits for it. So no call
// here may be entered as RawSyscall or RawSyscall6, from syscall or x/sys/unix.
func TestNoRawSyscalls(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
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
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && strings.HasPrefix(sel.Sel.Name, "RawSyscall") {
				t.Errorf("%s calls %s: a read of another process can sleep in the kernel, so enter it as a blocking Syscall",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no source files to check")
	}
}
