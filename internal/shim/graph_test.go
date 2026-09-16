package shim

import (
	"bytes"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestShimGraphAllowList pins the shim's dependency graph: the same
// binary is the `claude` shim, and every package it links runs its init
// on every launch. The shim may reach only the store, the resolver, the
// session dirs and the launch log — never the catalog, the bundle code,
// the TUI stack (charm.land) or the transfer crypto (x/crypto).
func TestShimGraphAllowList(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	cmd := exec.Command(goBin, "list", "-deps", ".")
	cmd.Env = os.Environ()
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, stderr.String())
	}

	const module = "github.com/jratienza65/bffs/"
	want := []string{
		module + "internal/projectconfig",
		module + "internal/resolver",
		module + "internal/sessions",
		module + "internal/shim",
		module + "internal/store",
		module + "internal/usagelog",
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		pkg := strings.TrimSpace(line)
		switch {
		case pkg == "":
		case strings.HasPrefix(pkg, module):
			got = append(got, pkg)
		case strings.HasPrefix(pkg, "charm.land/"):
			t.Errorf("shim links the TUI stack: %s", pkg)
		case strings.HasPrefix(pkg, "golang.org/x/crypto"):
			t.Errorf("shim links the transfer crypto: %s", pkg)
		}
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("shim internal deps changed:\n got %v\nwant %v", got, want)
	}
}
