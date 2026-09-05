package trust

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestTrustGraphAllowList pins the dependency rule of the plan's §4.2:
// trust may reach claudejson, transcripts, store, sessions and fsutil (and
// whatever those need) — never usage, mcpserver or cmd.
func TestTrustGraphAllowList(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	cmd := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}", ".")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, errb.String())
	}
	const module = "github.com/jratienza65/bffs/"
	allowed := map[string]bool{
		"internal/claudejson":  true,
		"internal/fsutil":      true,
		"internal/imports":     true,
		"internal/sessions":    true,
		"internal/store":       true,
		"internal/transcripts": true,
		"internal/trust":       true,
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, module) {
			continue
		}
		pkg := strings.TrimPrefix(line, module)
		if !allowed[pkg] {
			t.Errorf("trust must not depend on %s", pkg)
		}
	}
}
