// Package shimcheck answers one question: would a `claude` shim installed at a
// given path actually win on PATH?
//
// The naive way to answer it — read os.Getenv("PATH") — is wrong, and wrong in
// a way that hides real breakage. A user's PATH is assembled by shell startup
// files, and which files run depends on how the shell was invoked:
//
//	zsh   .zshenv (always) → .zprofile (login) → .zshrc (interactive)
//	bash  login: /etc/profile + ~/.bash_profile|~/.bash_login|~/.profile
//	      interactive non-login: ~/.bashrc
//	      neither: nothing at all (except $BASH_ENV)
//
// So a shim can win in an interactive terminal and lose in every other
// context — IDE integrations, launchd/systemd, cron, and `ssh host 'cmd'`
// all commonly run in one of the modes that reads fewer files. Checking the
// ambient PATH samples exactly the one environment where the shim is most
// likely to work, which is how a broken install passes its own check.
//
// This package instead probes each invocation mode in a subprocess and reports
// per-mode verdicts. Probes deliberately start from a minimal system PATH
// rather than inheriting the caller's, because the point is to model a fresh
// session, not the terminal bffs happens to be running in.
//
// Probing runs the user's own startup files. That is the only way to observe
// what they actually do to PATH — a static reading of the files cannot know
// that, say, a later `brew shellenv` re-prepends a directory ahead of an
// earlier edit.
package shimcheck

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Mode is a shell invocation mode. The set of startup files a shell reads —
// and therefore the PATH it ends up with — is a function of this.
type Mode string

const (
	// ModeInteractive is an interactive non-login shell: a new terminal tab.
	ModeInteractive Mode = "interactive"
	// ModeLogin is a login shell. Used by console logins, `ssh host`, and
	// commonly by GUI apps and IDEs that spawn a shell to learn your PATH.
	ModeLogin Mode = "login"
	// ModeNonInteractive is neither login nor interactive: `sh -c`, scripts,
	// cron, systemd units, and `ssh host 'cmd'`. bash reads no startup file
	// at all in this mode, which makes it the most common silent bypass.
	ModeNonInteractive Mode = "non-interactive"
)

// AllModes is the probe order, cheapest-to-surprise first.
var AllModes = []Mode{ModeInteractive, ModeLogin, ModeNonInteractive}

// Status is deliberately tri-state-plus: a boolean would force the Windows
// case (no shell startup files; PATH lives in the registry) to report a
// verdict it cannot actually determine.
type Status string

const (
	// StatusWins: the shim resolves first for this mode.
	StatusWins Status = "wins"
	// StatusShadowed: a different `claude` resolves first. The dangerous
	// case — everything looks installed, and the shim silently never runs.
	// ModeResult.InstallDirOnPath distinguishes "on PATH but too late" from
	// "not on PATH at all", which changes the remedy wording, not the verdict.
	StatusShadowed Status = "shadowed"
	// StatusNotOnPath: nothing named `claude` resolves at all in this mode.
	StatusNotOnPath Status = "not-on-PATH"
	// StatusUnknown: could not be determined (unsupported shell, probe
	// failure, Windows). Never treated as success.
	StatusUnknown Status = "unknown"
)

// OK reports whether the shim would actually be used in this mode.
func (s Status) OK() bool { return s == StatusWins }

// ModeResult is the verdict for one shell invocation mode.
type ModeResult struct {
	Mode Mode
	// Status is the verdict.
	Status Status
	// Blocker is the competing `claude` that wins instead, when Shadowed.
	Blocker string
	// InstallDirOnPath records whether the install dir appears in this
	// mode's PATH at all — "present but too late" and "absent" are both
	// failures but call for different advice.
	InstallDirOnPath bool
	// PATH is the effective PATH observed for this mode (empty if Unknown).
	PATH string
	// Note explains an Unknown, or adds context to a negative verdict.
	Note string
}

// Report is the full picture for one candidate install directory.
type Report struct {
	// Shell is the shell that was probed (basename of $SHELL).
	Shell string
	// InstallDir is the directory the shim would live in.
	InstallDir string
	// ShimName is the binary name being shadowed ("claude"/"claude.exe").
	ShimName string
	// Modes holds one result per probed mode.
	Modes []ModeResult
	// Ambient is the verdict against the PATH bffs itself is running with.
	// Kept separate because it is not representative of a fresh session —
	// it is only meaningful for "will this work right now, in this terminal".
	Ambient ModeResult
}

// Blocked reports whether any probed mode would fail to use the shim. Unknown
// does not count as blocked (we cannot claim breakage we did not observe),
// but it does not count as passing either — see AllWin.
func (r Report) Blocked() bool {
	for _, m := range r.Modes {
		if m.Status == StatusShadowed || m.Status == StatusNotOnPath {
			return true
		}
	}
	return false
}

// AllWin reports whether every probed mode resolves to the shim.
func (r Report) AllWin() bool {
	if len(r.Modes) == 0 {
		return false
	}
	for _, m := range r.Modes {
		if !m.Status.OK() {
			return false
		}
	}
	return true
}

// Failing returns the modes that would not use the shim.
func (r Report) Failing() []ModeResult {
	var out []ModeResult
	for _, m := range r.Modes {
		if m.Status == StatusShadowed || m.Status == StatusNotOnPath {
			out = append(out, m)
		}
	}
	return out
}

// Analyze decides, for one already-observed PATH, whether a shim living in
// installDir would be the `claude` that runs.
//
// It is a pure function of (pathValue, installDir, selfPath) plus filesystem
// lookups, so it works identically before and after the shim is installed:
// walking PATH in order, whichever comes first — the install dir or some
// other real `claude` — decides the outcome.
//
// selfPath is the resolved bffs binary; a `claude` on PATH that resolves to
// it is our own shim, not a competitor.
func Analyze(pathValue, installDir, selfPath, shimName string) ModeResult {
	res := ModeResult{PATH: pathValue}
	dirs := filepath.SplitList(pathValue)
	for _, dir := range dirs {
		if pathsEqual(dir, installDir) {
			res.InstallDirOnPath = true
			break
		}
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if pathsEqual(dir, installDir) {
			res.Status = StatusWins
			return res
		}
		candidate := filepath.Join(dir, shimName)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || !isExecutable(info) {
			continue
		}
		// Our own shim already sitting somewhere else on PATH still means a
		// bffs-managed claude runs, so this is not a block.
		if resolvesTo(candidate, selfPath) {
			res.Status = StatusWins
			return res
		}
		res.Status = StatusShadowed
		res.Blocker = candidate
		return res
	}
	res.Status = StatusNotOnPath
	return res
}

// Prober observes the PATH a shell ends up with for a given invocation mode.
type Prober interface {
	PATH(ctx context.Context, mode Mode) (string, error)
}

// Options configures Check.
type Options struct {
	InstallDir string
	ShimName   string
	// SelfPath is the resolved bffs binary.
	SelfPath string
	// Shell overrides the shell to probe (defaults to $SHELL).
	Shell string
	// Prober overrides the probe mechanism (tests inject a fake).
	Prober Prober
	// Timeout bounds each probe. A pathological startup file must not wedge
	// `bffs init`.
	Timeout time.Duration
}

// Check probes each invocation mode and returns the combined report.
func Check(ctx context.Context, opts Options) Report {
	if opts.ShimName == "" {
		opts.ShimName = DefaultShimName()
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	shell := opts.Shell
	if shell == "" {
		shell = filepath.Base(os.Getenv("SHELL"))
	}

	r := Report{
		Shell:      shell,
		InstallDir: opts.InstallDir,
		ShimName:   opts.ShimName,
		Ambient:    Analyze(os.Getenv("PATH"), opts.InstallDir, opts.SelfPath, opts.ShimName),
	}
	r.Ambient.Mode = "current shell"

	prober := opts.Prober
	if prober == nil {
		p, note := newShellProber(shell, opts.Timeout)
		if p == nil {
			// Cannot probe: report Unknown for every mode rather than
			// guessing. Callers must not read Unknown as success.
			for _, m := range AllModes {
				r.Modes = append(r.Modes, ModeResult{Mode: m, Status: StatusUnknown, Note: note})
			}
			return r
		}
		prober = p
	}

	for _, m := range AllModes {
		pathValue, err := prober.PATH(ctx, m)
		if err != nil {
			r.Modes = append(r.Modes, ModeResult{Mode: m, Status: StatusUnknown, Note: err.Error()})
			continue
		}
		res := Analyze(pathValue, opts.InstallDir, opts.SelfPath, opts.ShimName)
		res.Mode = m
		r.Modes = append(r.Modes, res)
	}
	return r
}

// DefaultShimName is the binary name the shim installs as.
func DefaultShimName() string {
	if runtime.GOOS == "windows" {
		return "claude.exe"
	}
	return "claude"
}

// shellProber runs `<shell> <flags> -c 'printf %s "$PATH"'` per mode.
type shellProber struct {
	shell   string
	timeout time.Duration
}

// posixModeFlags maps a mode to the flags that produce it. Only shells whose
// startup-file semantics we actually know are probed; anything else reports
// Unknown rather than a confident guess.
var posixModeFlags = map[Mode][]string{
	ModeInteractive:    {"-i"},
	ModeLogin:          {"-l"},
	ModeNonInteractive: nil,
}

// knownPOSIXShells are the shells whose -i/-l semantics match the table above.
// fish is deliberately excluded: its startup model differs (config.fish is
// interactive-only) and guessing would produce confidently wrong verdicts.
var knownPOSIXShells = map[string]bool{
	"bash": true, "zsh": true, "sh": true, "dash": true, "ksh": true, "mksh": true,
}

func newShellProber(shell string, timeout time.Duration) (Prober, string) {
	if runtime.GOOS == "windows" {
		return nil, "PATH on Windows comes from the registry, not shell startup files; verify manually"
	}
	if shell == "" {
		return nil, "$SHELL is not set, so there is no login shell to probe"
	}
	if !knownPOSIXShells[shell] {
		return nil, fmt.Sprintf("startup-file behaviour of %q is not modelled; verify manually", shell)
	}
	if _, err := exec.LookPath(shell); err != nil {
		return nil, fmt.Sprintf("%s not found on PATH: %v", shell, err)
	}
	return &shellProber{shell: shell, timeout: timeout}, ""
}

func (p *shellProber) PATH(ctx context.Context, mode Mode) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	args := append(append([]string{}, posixModeFlags[mode]...), "-c", `printf %s "$PATH"`)
	cmd := exec.CommandContext(ctx, p.shell, args...)
	cmd.Env = probeEnv()
	// Startup files chatter (motd, greetings, compinit warnings); only stdout
	// carries the answer.
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s %s probe timed out after %s (slow startup file?)", p.shell, mode, p.timeout)
	}
	if err != nil {
		return "", fmt.Errorf("%s %s probe failed: %w", p.shell, mode, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// probeEnv builds the environment for a probe. It deliberately does NOT
// inherit the caller's PATH: the question is what a fresh session would see,
// and inheriting would let the current terminal's PATH mask the problem —
// the exact failure mode this package exists to catch.
func probeEnv() []string {
	env := []string{
		"HOME=" + os.Getenv("HOME"),
		"USER=" + os.Getenv("USER"),
		"SHELL=" + os.Getenv("SHELL"),
		"LANG=" + os.Getenv("LANG"),
		// Discourage interactive startup files from drawing prompts.
		"TERM=dumb",
		"PATH=" + systemDefaultPATH(),
	}
	// zsh reads its startup files from ZDOTDIR when set; dropping it would
	// probe the wrong files entirely.
	if v := os.Getenv("ZDOTDIR"); v != "" {
		env = append(env, "ZDOTDIR="+v)
	}
	return env
}

// systemDefaultPATH approximates the PATH a session gets before any user
// startup file runs — close to what sshd and launchd hand out.
func systemDefaultPATH() string {
	return "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

// Render writes a human-readable report.
func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "Would a %s shim in %s actually run?\n\n", r.ShimName, r.InstallDir)
	for _, m := range r.Modes {
		fmt.Fprintf(w, "  %-16s %s\n", m.Mode, describe(m))
	}
	fmt.Fprintf(w, "  %-16s %s\n", r.Ambient.Mode, describe(r.Ambient))
	if r.Shell != "" {
		fmt.Fprintf(w, "\n  (probed %s; modes differ because each reads different startup files)\n", r.Shell)
	}
}

func describe(m ModeResult) string {
	switch m.Status {
	case StatusWins:
		return "yes"
	case StatusShadowed:
		if m.InstallDirOnPath {
			return fmt.Sprintf("NO — %s comes first on PATH", m.Blocker)
		}
		return fmt.Sprintf("NO — %s wins; install dir is not on PATH here", m.Blocker)
	case StatusNotOnPath:
		return "NO — no claude resolves at all in this mode"
	default:
		note := m.Note
		if note == "" {
			note = "could not determine"
		}
		return "unknown — " + note
	}
}

// resolvesTo reports whether candidate is really selfPath. Both sides are
// resolved: callers may pass an unresolved selfPath, and on macOS symlinked
// prefixes (/var → /private/var) would otherwise defeat the comparison.
func resolvesTo(candidate, selfPath string) bool {
	if selfPath == "" {
		return false
	}
	a, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false
	}
	b, err := filepath.EvalSymlinks(selfPath)
	if err != nil {
		b = selfPath
	}
	return pathsEqual(a, b)
}

func isExecutable(info os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SafeDirs suggests install directories that would make the shim win in every
// mode that currently fails. A directory qualifies only if it appears — ahead
// of any competing claude — in every probed mode's PATH, and is writable
// without elevation.
//
// This exists because the collision-safe default (a dedicated bffs dir) is by
// construction on nobody's PATH, so "use the default" always requires a PATH
// edit. When the machine already has a writable directory early enough on
// PATH in every mode, pointing the shim there works immediately instead.
func (r Report) SafeDirs(selfPath string) []string {
	usable := map[string]int{} // dir → number of modes where it is early enough
	modes := 0
	for _, m := range r.Modes {
		if m.Status == StatusUnknown || m.PATH == "" {
			continue
		}
		modes++
		for _, dir := range filepath.SplitList(m.PATH) {
			if dir == "" {
				continue
			}
			// Stop at the first competing claude: anything at or after it is
			// too late to help.
			candidate := filepath.Join(dir, r.ShimName)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && isExecutable(info) {
				if !resolvesTo(candidate, selfPath) {
					break
				}
			}
			usable[filepath.Clean(dir)]++
		}
	}
	if modes == 0 {
		return nil
	}
	var out []string
	for dir, n := range usable {
		if n != modes || !writable(dir) {
			continue
		}
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// writable reports whether a directory exists and accepts new files without
// elevation. Probing with a real create is the only portable answer: mode
// bits alone miss ACLs, read-only mounts, and ownership.
func writable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	f, err := os.CreateTemp(dir, ".bffs-write-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
