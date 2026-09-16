package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWrapperScriptHandlesShellMetachars verifies that the fallback
// shim-wrapper script treats `self` as a shell-literal even when the path
// contains characters the shell would otherwise expand ($, `, ", \, etc.).
//
// The current implementation uses fmt.Sprintf("%q", self) which produces a
// Go-style double-quoted string. /bin/sh interprets that as a double-quoted
// argument with parameter / command / backslash expansion — so a path like
// `/opt/$EVIL/bin` would expand $EVIL inside the running script, executing
// the wrong binary (or nothing at all).
//
// Fix is to single-quote escape (with the standard '\'' trick for embedded
// single quotes), or shell-escape via a dedicated helper.
func TestWrapperScriptHandlesShellMetachars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper script is /bin/sh; windows uses a copy of the .exe instead")
	}

	cases := []struct {
		name string
		self string
	}{
		{"dollar sign", `/opt/$USER/bin/bffs`},
		{"backtick", "/opt/`whoami`/bin/bffs"},
		{"single quote", `/opt/o'reilly/bin/bffs`},
		{"backslash", `/opt/back\slash/bin/bffs`},
		{"double quote", `/opt/say"hi/bin/bffs`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := wrapperScript(tc.self)

			// Replace the `exec ` token with a sh function that prints ONLY
			// argv[1] (the binary path) and stops, so we observe how the
			// shell parses the first quoted word. If $ / backtick / etc.
			// caused the shell to expand the path, the printed value will
			// not match tc.self — that's the bug we're trying to catch.
			modified := "p() { printf '%s' \"$1\"; }\n" + replaceExec(script, "p ")
			out, err := exec.Command("/bin/sh", "-c", modified).Output()
			if err != nil {
				t.Fatalf("sh failed to parse the script: %v\nscript was:\n%s", err, modified)
			}
			got := string(out)
			if got != tc.self {
				t.Errorf("shell expanded the wrapper's path:\n  want: %q\n  got:  %q\n  script:\n%s", tc.self, got, script)
			}
		})
	}
}

// replaceExec swaps the literal token `exec ` for `with`, leaving the rest
// of the line (the quoted self path + " exec -- \"$@\"") verbatim so the
// shell still parses it the same way.
func replaceExec(script, with string) string {
	const old = "exec "
	i := indexLine(script, old)
	if i < 0 {
		return script
	}
	return script[:i] + with + script[i+len(old):]
}

func indexLine(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// silence "imported and not used" if a future refactor drops a helper.
var _ = filepath.Join
var _ = os.WriteFile
