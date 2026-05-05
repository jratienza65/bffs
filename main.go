package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jratienza65/bffs/cmd"
	"github.com/jratienza65/bffs/internal/shim"
)

var modes = map[string]func(){
	"claude": func() {
		exit, err := shim.Run(os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "bffs shim:", err)
			if exit == 0 {
				exit = 1
			}
		}
		os.Exit(exit)
	},
}

func main() {
	if run, ok := modes[shimMode(os.Args[0])]; ok {
		run()
		return
	}
	cmd.Execute()
}

// shimMode inspects argv[0] and returns "claude" if we were invoked under that
// name (with or without a .exe suffix), so the multi-call binary can dispatch.
func shimMode(arg0 string) string {
	base := strings.ToLower(filepath.Base(arg0))
	base = strings.TrimSuffix(base, ".exe")
	if base == "claude" {
		return "claude"
	}
	return ""
}
