package transcripts

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

// envConfigDirPrefix is the one environment entry LiveAccounts keeps from
// a process's environment block.
const envConfigDirPrefix = EnvClaudeConfigDir + "="

// procEnviron returns the NUL-separated environment block of pid (darwin:
// the environment part of kern.procargs2; linux: /proc/<pid>/environ) or
// an error when the platform or the process's owner does not allow it. It
// is a variable so tests can inject a block. The block holds every secret
// the process was launched with (API keys, tokens); callers must extract
// what they need and drop it, never log or return it.
var procEnviron = readProcEnviron

// maxProcEnviron bounds the environment block read for one process.
const maxProcEnviron = 4 << 20

// errProcEnvironUnsupported is what readProcEnviron returns where bffs has
// no way to read another process's environment.
var errProcEnvironUnsupported = errors.New("process environment lookup not supported on this platform")

// LiveAccounts returns live with Account and AccountKnown filled for every
// session, best-effort: the process's environment is read (procEnviron),
// only its CLAUDE_CONFIG_DIR entry is kept, and that directory is matched
// (symlinks resolved) against sessions.Dir(cfgDir, name) for every oauth
// account in accs. A process whose environment cannot be read — another
// user's, or on Windows — keeps AccountKnown false. Nothing else from the
// environment is retained, and no value from it reaches a field or an
// error. live itself is not modified.
func LiveAccounts(live map[string]LiveSession, cfgDir string, accs store.Accounts) map[string]LiveSession {
	dirs := map[string]string{}
	for name, a := range accs.Accounts {
		if a.Type != store.TypeOAuth {
			continue
		}
		dirs[canonical(sessions.Dir(cfgDir, name))] = name
	}
	out := make(map[string]LiveSession, len(live))
	for sid, s := range live {
		s.Account, s.AccountKnown = accountOfPID(s.PID, dirs)
		out[sid] = s
	}
	return out
}

// accountOfPID maps pid's CLAUDE_CONFIG_DIR to an account name via dirs
// (canonical session dir → account). The environment block is scrubbed
// before returning.
func accountOfPID(pid int, dirs map[string]string) (account string, known bool) {
	if pid <= 0 {
		return "", false
	}
	block, err := procEnviron(pid)
	if err != nil {
		return "", false
	}
	dir, found := configDirFromEnviron(block)
	clear(block)
	if !found || dir == "" {
		return HomeName, true
	}
	if name, ok := dirs[canonical(dir)]; ok {
		return name, true
	}
	return "", true
}

// configDirFromEnviron returns the value of the first CLAUDE_CONFIG_DIR
// entry in a NUL-separated environment block. The value is copied, so the
// caller may clear the block.
func configDirFromEnviron(block []byte) (string, bool) {
	for len(block) > 0 {
		entry := block
		if i := bytes.IndexByte(block, 0); i >= 0 {
			entry, block = block[:i], block[i+1:]
		} else {
			block = nil
		}
		if bytes.HasPrefix(entry, []byte(envConfigDirPrefix)) {
			return string(entry[len(envConfigDirPrefix):]), true
		}
	}
	return "", false
}

// procargs2Environ extracts the environment block from a darwin
// kern.procargs2 buffer: a native-endian int32 argc, the executable path,
// NUL padding, argc argument strings, then the environment strings, each
// NUL-terminated, ending at an empty string. The result aliases buf.
func procargs2Environ(buf []byte) ([]byte, error) {
	if len(buf) < 4 {
		return nil, errors.New("procargs2: short buffer")
	}
	argc := int(binary.NativeEndian.Uint32(buf[:4]))
	rest := buf[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, errors.New("procargs2: no executable path")
	}
	rest = rest[i+1:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for k := 0; k < argc; k++ {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil, errors.New("procargs2: truncated arguments")
		}
		rest = rest[i+1:]
	}
	// The environment runs to the first empty string (two NULs in a row)
	// or the end of the buffer.
	end := len(rest)
	for pos := 0; pos < len(rest); {
		i := bytes.IndexByte(rest[pos:], 0)
		if i < 0 {
			break
		}
		if i == 0 {
			end = pos
			break
		}
		pos += i + 1
	}
	return rest[:end], nil
}
