package transcripts

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

const plantedSecret = "sk-ant-api03-PLANTED-SECRET-VALUE"

// fakeEnviron builds a NUL-separated environment block.
func fakeEnviron(entries ...string) []byte {
	return []byte(strings.Join(entries, "\x00") + "\x00")
}

func TestLiveAccounts(t *testing.T) {
	cfg := t.TempDir()
	accs := store.Accounts{Accounts: map[string]store.Account{
		"work":  {Type: store.TypeOAuth},
		"play":  {Type: store.TypeOAuth},
		"keyed": {Type: store.TypeAPIKey, Secret: plantedSecret},
	}}
	workDir := sessions.Dir(cfg, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlink to the account dir must resolve to the same account.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(workDir, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	blocks := map[int][]byte{
		1: fakeEnviron("PATH=/usr/bin", "ANTHROPIC_API_KEY="+plantedSecret, envConfigDirPrefix+workDir, "HOME=/Users/x"),
		2: fakeEnviron("PATH=/usr/bin", envConfigDirPrefix+link),
		3: fakeEnviron("PATH=/usr/bin", "ANTHROPIC_API_KEY="+plantedSecret, "HOME=/Users/x"),
		4: fakeEnviron(envConfigDirPrefix + filepath.Join(t.TempDir(), "elsewhere")),
		5: fakeEnviron(envConfigDirPrefix),
	}
	orig := procEnviron
	t.Cleanup(func() { procEnviron = orig })
	procEnviron = func(pid int) ([]byte, error) {
		b, ok := blocks[pid]
		if !ok {
			return nil, fmt.Errorf("pid %d: %w", pid, os.ErrPermission)
		}
		return append([]byte(nil), b...), nil
	}

	live := map[string]LiveSession{
		"s1": {PID: 1, SessionID: "s1", Cwd: "/p", ConfigDir: cfg, Verified: true},
		"s2": {PID: 2, SessionID: "s2"},
		"s3": {PID: 3, SessionID: "s3"},
		"s4": {PID: 4, SessionID: "s4"},
		"s5": {PID: 5, SessionID: "s5"},
		"s6": {PID: 6, SessionID: "s6"},
		"s7": {PID: 0, SessionID: "s7"},
	}
	got := LiveAccounts(live, cfg, accs)
	want := map[string][2]any{
		"s1": {"work", true},
		"s2": {"work", true},
		"s3": {HomeName, true},
		"s4": {"", true},
		"s5": {HomeName, true},
		"s6": {"", false},
		"s7": {"", false},
	}
	for sid, w := range want {
		s := got[sid]
		if s.Account != w[0] || s.AccountKnown != w[1] {
			t.Errorf("%s: Account=%q Known=%v, want %q %v", sid, s.Account, s.AccountKnown, w[0], w[1])
		}
	}
	if s := got["s1"]; s.PID != 1 || s.Cwd != "/p" || s.ConfigDir != cfg || !s.Verified {
		t.Errorf("other fields changed: %+v", s)
	}
	if live["s1"].AccountKnown {
		t.Error("input map modified")
	}
	// The planted secret reaches no field, in no rendering of the result.
	if text := fmt.Sprintf("%+v %#v", got, got); strings.Contains(text, plantedSecret) || strings.Contains(text, "PLANTED") {
		t.Errorf("secret leaked into the result: %s", text)
	}
}

func TestAccountOfPIDErrorsCarryNoEnvironment(t *testing.T) {
	orig := procEnviron
	t.Cleanup(func() { procEnviron = orig })
	// A reader that fails after producing a block: neither the block nor
	// its content may surface anywhere. accountOfPID has no error return
	// by design; this pins that its only outputs are the two flags.
	procEnviron = func(pid int) ([]byte, error) {
		return fakeEnviron("ANTHROPIC_API_KEY=" + plantedSecret), errors.New("read failed")
	}
	account, known := accountOfPID(42, map[string]string{})
	if account != "" || known {
		t.Errorf("accountOfPID on error = %q, %v", account, known)
	}
	if strings.Contains(errProcEnvironUnsupported.Error(), plantedSecret) {
		t.Error("sentinel carries a secret")
	}
}

func TestConfigDirFromEnviron(t *testing.T) {
	cases := []struct {
		block []byte
		want  string
		found bool
	}{
		{fakeEnviron("A=1", envConfigDirPrefix+"/x/y", "B=2"), "/x/y", true},
		{fakeEnviron("A=1", "B=2"), "", false},
		{[]byte(envConfigDirPrefix + "/no/trailing/nul"), "/no/trailing/nul", true},
		{fakeEnviron("XCLAUDE_CONFIG_DIR=/not/it", "CLAUDE_CONFIG_DIRS=/not/it/either"), "", false},
		{fakeEnviron(envConfigDirPrefix+"/first", envConfigDirPrefix+"/second"), "/first", true},
		{nil, "", false},
	}
	for _, c := range cases {
		got, found := configDirFromEnviron(c.block)
		if got != c.want || found != c.found {
			t.Errorf("configDirFromEnviron(%q) = (%q, %v), want (%q, %v)", c.block, got, found, c.want, c.found)
		}
	}
}

func TestProcargs2Environ(t *testing.T) {
	var buf []byte
	buf = binary.NativeEndian.AppendUint32(buf, 2) // argc
	buf = append(buf, "/usr/local/bin/claude\x00\x00\x00\x00"...)
	buf = append(buf, "claude\x00--resume\x00"...)
	env := "PATH=/usr/bin\x00" + envConfigDirPrefix + "/cfg/sessions/work\x00ANTHROPIC_API_KEY=" + plantedSecret + "\x00"
	buf = append(buf, env...)
	buf = append(buf, "\x00trailing-junk\x00"...)
	got, err := procargs2Environ(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != env {
		t.Errorf("environ = %q, want %q", got, env)
	}
	dir, found := configDirFromEnviron(got)
	if !found || dir != "/cfg/sessions/work" {
		t.Errorf("config dir = %q, %v", dir, found)
	}
	// An argument that looks like the variable is not the environment.
	buf = binary.NativeEndian.AppendUint32(nil, 1)
	buf = append(buf, "/bin/x\x00"+envConfigDirPrefix+"/from/argv\x00HOME=/h\x00\x00"...)
	got, err = procargs2Environ(buf)
	if err != nil {
		t.Fatal(err)
	}
	if dir, found := configDirFromEnviron(got); found || dir != "" {
		t.Errorf("argv leaked into the environment: %q", got)
	}
	for _, bad := range [][]byte{nil, {1, 0, 0}, binary.NativeEndian.AppendUint32(nil, 3), append(binary.NativeEndian.AppendUint32(nil, 3), "/bin/x\x00a\x00"...)} {
		if _, err := procargs2Environ(bad); err == nil {
			t.Errorf("procargs2Environ(%q) accepted", bad)
		}
	}
}

func TestLiveAccountsChildProcess(t *testing.T) {
	// The real reader against a child of this process: readable on darwin
	// (kern.procargs2) and linux (/proc), and the planted variable must not
	// leak anywhere. Unsupported platforms report the account unknown.
	cfg := t.TempDir()
	accs := store.Accounts{Accounts: map[string]store.Account{"me": {Type: store.TypeOAuth}}}
	if _, err := readProcEnviron(os.Getpid()); errors.Is(err, errProcEnvironUnsupported) {
		got := LiveAccounts(map[string]LiveSession{"s": {PID: os.Getpid(), SessionID: "s"}}, cfg, accs)
		if s := got["s"]; s.AccountKnown || s.Account != "" {
			t.Errorf("unsupported platform reported %+v", s)
		}
		return
	}
	// macOS hides the environment of platform binaries (a /bin/sleep child
	// shows no environment), so the child is this test binary itself.
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := exec.CommandContext(ctx, self, "-test.run=^TestHelperProcess$")
	child.Env = append(os.Environ(), helperSleepEnv+"=1", EnvClaudeConfigDir+"="+sessions.Dir(cfg, "me"), "BFFS_TEST_PLANTED="+plantedSecret)
	child.Stdin, child.Stdout, child.Stderr = nil, io.Discard, io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = child.Wait() }()

	got := LiveAccounts(map[string]LiveSession{"s": {PID: child.Process.Pid, SessionID: "s"}}, cfg, accs)
	if s := got["s"]; !s.AccountKnown || s.Account != "me" {
		t.Errorf("child process: %+v", s)
	}
	if text := fmt.Sprintf("%+v", got); strings.Contains(text, plantedSecret) {
		t.Errorf("secret leaked: %s", text)
	}
}

// helperSleepEnv makes TestHelperProcess, run in a child test binary, sleep
// so the parent can read its environment.
const helperSleepEnv = "BFFS_TEST_HELPER_SLEEP"

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperSleepEnv) == "" {
		t.Skip("not a helper process")
	}
	time.Sleep(30 * time.Second)
}
