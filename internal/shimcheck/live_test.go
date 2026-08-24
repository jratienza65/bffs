package shimcheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLiveProbe runs the real probe against the caller's actual shell and
// startup files. It is environment-dependent and spawns shells, so it is
// opt-in rather than part of the normal suite:
//
//	BFFS_LIVE_PROBE=1 go test ./internal/shimcheck -run TestLiveProbe -v
//
// It asserts nothing about the outcome — the point is to print what a real
// machine reports, which is how you confirm the probe sees what your shells
// actually do.
func TestLiveProbe(t *testing.T) {
	if os.Getenv("BFFS_LIVE_PROBE") == "" {
		t.Skip("set BFFS_LIVE_PROBE=1 to probe this machine's real shell startup files")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		filepath.Join(home, ".bffs", "bin"),
		filepath.Join(home, ".local", "bin"),
	} {
		rep := Check(context.Background(), Options{InstallDir: dir, SelfPath: ""})
		t.Logf("\n%s", render(rep))
	}
}

func render(r Report) string {
	var b builder
	r.Render(&b)
	return b.String()
}

type builder struct{ b []byte }

func (w *builder) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }
func (w *builder) String() string              { return string(w.b) }
