// gen-design runs independently of the af command tree and all user state.
package main

import (
	"flag"
	"fmt"
	"github.com/sachiniyer/agent-factory/internal/designtokens"
	"os"
)

func main() {
	check := flag.Bool("check", false, "Check committed output without writing")
	root := flag.String("root", ".", "Repository root")
	flag.Parse()
	out, err := designtokens.Generate(*root)
	if err == nil {
		if *check {
			err = designtokens.Check(*root, out)
		} else {
			err = designtokens.Write(*root, out)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
