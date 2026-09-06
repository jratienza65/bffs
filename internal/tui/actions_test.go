package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

// loopbackLinks is the address set both sides see in the loopback tests.
func loopbackLinks() []transfer.LinkAddr {
	return []transfer.LinkAddr{{Addr: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8"), Iface: "lo"}}
}

// loopbackTransfer points both sides at loopback the way cmd's tests do:
// the serve binds 127.0.0.1 on a kernel-chosen port and reports the bound
// address, both address sets are loopback, loopback peers are admitted,
// the code is the one given and the countdown ticks fast. Everything is
// restored at cleanup.
func loopbackTransfer(t *testing.T, code transfer.Code) <-chan netip.AddrPort {
	t.Helper()
	addrCh := make(chan netip.AddrPort, 1)
	oldListen, oldLocal, oldGen, oldFetchLocal := serveListen, serveLocal, serveGenerate, fetchLocal
	oldLAN, oldPort, oldTick := lanOptions, serveBindPort, tickEvery
	serveListen = func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if err == nil {
			select {
			case addrCh <- netip.MustParseAddrPort(ln.Addr().String()):
			default:
			}
		}
		return ln, err
	}
	lo := func(transfer.LANOptions) ([]transfer.LinkAddr, error) { return loopbackLinks(), nil }
	serveLocal, fetchLocal = lo, lo
	serveGenerate = func() (transfer.Code, error) { return code, nil }
	lanOptions = transfer.LANOptions{AllowLoopback: true}
	serveBindPort = 0
	tickEvery = time.Millisecond
	t.Cleanup(func() {
		serveListen, serveLocal, serveGenerate, fetchLocal = oldListen, oldLocal, oldGen, oldFetchLocal
		lanOptions, serveBindPort, tickEvery = oldLAN, oldPort, oldTick
	})
	return addrCh
}

func mustParseCode(t *testing.T, s string) transfer.Code {
	t.Helper()
	c, err := transfer.ParseCode(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// services builds the shared services over the fixture with the fixed
// clock, for tests that drive one screen directly.
func (f *fixture) services() *services {
	f.t.Helper()
	f.saveAccounts()
	svc, err := loadServices(context.Background(), Options{CfgDir: f.cfgDir, HomeClaudeDir: f.claudeDir, Version: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	svc.now = func() time.Time { return fixedNow }
	return svc
}

// homeRoot is the fixture's ~/.claude pool.
func (f *fixture) homeRoot(svc *services) transcripts.Root {
	f.t.Helper()
	root, err := transcripts.RootFor(svc.roots, transcripts.HomeName)
	if err != nil {
		f.t.Fatal(err)
	}
	return root
}

// openSessions starts the harness with the sessions panel focused on
// the fixture's project.
func (f *fixture) openSessions() (*harness, *workspace) {
	f.t.Helper()
	h := f.start("sessions")
	h.keys("3")
	ws := h.a.ws
	if ws.focus != panelItems || ws.tab != tabSessions || h.a.top() != nil {
		f.t.Fatalf("3 should focus the sessions panel: focus=%d tab=%d top=%T", ws.focus, ws.tab, h.a.top())
	}
	return h, ws
}

// drive feeds msg to the screen and runs the commands it returns the way
// the harness does, minus the app: every message flows back until the
// screen stops producing them or emits a navigation message, which is
// returned. Tick messages are dropped (their command is run, never fed).
func drive(t *testing.T, s Screen, msg tea.Msg) tea.Msg {
	t.Helper()
	var pending []tea.Cmd
	feed := func(m tea.Msg) tea.Msg {
		switch m := m.(type) {
		case nil:
			return nil
		case tea.BatchMsg:
			pending = append(pending, m...)
			return nil
		case tickMsg:
			return nil
		case pushScreenMsg, popScreenMsg, replaceScreenMsg, statusMsg, quitMsg:
			return m
		}
		_, cmd := s.Update(m)
		if cmd != nil {
			pending = append(pending, cmd)
		}
		return nil
	}
	if nav := feed(msg); nav != nil {
		return nav
	}
	for len(pending) > 0 {
		cmd := pending[0]
		pending = pending[1:]
		if nav := feed(cmd()); nav != nil {
			return nav
		}
	}
	return nil
}

// The export screen refuses an existing path, then writes a bundle of
// the project into a fresh one; the result names the path and the id.
func TestExportFileScreen(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	h, _ := f.openSessions()
	h.keys("e")
	sc, ok := h.a.top().(*exportScreen)
	if !ok {
		t.Fatalf("e should open the export screen, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "export the whole project "+shortPath(f.project)+" and its memory", "to: ", ".bffs", "an existing file is never overwritten")
	if !strings.Contains(sc.input.Value(), filepath.Join(f.project, "bffs-")) {
		t.Errorf("default path not in the cwd: %q", sc.input.Value())
	}

	// An existing file is refused before any selection work.
	taken := filepath.Join(f.project, "taken.bffs")
	if err := os.WriteFile(taken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc.input.SetValue(taken)
	h.keys("enter")
	wantAll(t, h.view(), "taken.bffs exists; choose another name")
	if sc.state != exportInput || h.a.top() != sc {
		t.Fatalf("state = %v top = %T", sc.state, h.a.top())
	}

	// A fresh path: the summary is shown, y writes the bundle.
	path := filepath.Join(f.project, "out.bffs")
	sc.input.SetValue(path)
	h.keys("enter")
	if sc.state != exportConfirm {
		t.Fatalf("after enter: state = %v, view:\n%s", sc.state, h.view())
	}
	out = h.view()
	wantAll(t, out, "Exporting from the shared pool", "partial isolation: work", "project "+shortPath(f.project), "1 session", "first prompt of one",
		"tool-results", "memory        2 files", "total", "Write "+shortPath(path)+"? [y/N]")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("bundle written before the confirmation")
	}
	h.keys("y")
	rs, ok := h.a.top().(*resultScreen)
	if !ok {
		t.Fatalf("y should end on the result screen, got %T:\n%s", h.a.top(), h.view())
	}
	if rs.err != nil {
		t.Fatalf("export failed: %v", rs.err)
	}
	wantAll(t, h.view(), "wrote "+shortPath(path), "1 session", "2 memory files", "bundle ")
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	m, _, _, err := bundle.PeekManifest(fh)
	if err != nil {
		t.Fatalf("PeekManifest: %v", err)
	}
	if n, mem := manifestCounts(m); n != 1 || mem != 2 || m.Source.Account != "" {
		t.Errorf("manifest: sessions=%d memory files=%d account=%q", n, mem, m.Source.Account)
	}
	wantAll(t, h.view(), short8(m.BundleID))
	// esc closes the result and reloads the sessions underneath.
	h.keys("esc")
	if h.a.top() != nil {
		t.Fatalf("esc on the result should pop to the workspace, got %T", h.a.top())
	}
	wantAll(t, h.view(), "first prompt of one")
}

// The copy screen greys every destination that shares the source pool
// and copies into a full-isolation account's own root.
func TestCopyScreen(t *testing.T) {
	f := newFixture(t)
	f.accounts.Accounts["full"] = store.Account{Type: store.TypeOAuth, Isolation: store.IsolationFull}
	fullProjects := filepath.Join(f.cfgDir, "sessions", "full", "projects")
	if err := os.MkdirAll(fullProjects, 0o755); err != nil {
		t.Fatal(err)
	}
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	h := f.start("sessions")
	h.keys("3") // the home root's project, sessions panel
	if h.a.top() != nil || h.a.ws.root.Owner != "" {
		t.Fatalf("expected the home root's sessions panel, got %T root=%q", h.a.top(), h.a.ws.root.Owner)
	}
	h.keys("c")
	sc, ok := h.a.top().(*copyScreen)
	if !ok {
		t.Fatalf("c should open the copy screen, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "copy the whole project", "ACCOUNT", "DESTINATION",
		"work", "oauth/partial", "already shared under partial isolation — run trust sync (t)",
		"home", "full", "oauth/full", "account: full")
	var greyed, selectable int
	for _, r := range sc.rows {
		if r.note != "" {
			greyed++
		} else {
			selectable++
		}
	}
	if greyed != 2 || selectable != 1 || sc.rows[sc.cursor].name != "full" {
		t.Fatalf("rows = %+v cursor=%d", sc.rows, sc.cursor)
	}
	rowOf := func(name string) int {
		for i, r := range sc.rows {
			if r.name == name {
				return i
			}
		}
		t.Fatalf("no row %q in %+v", name, sc.rows)
		return -1
	}
	// A greyed row explains itself and stays put.
	sc.cursor = rowOf("work")
	h.keys("enter")
	wantAll(t, h.view(), "work: already shared under partial isolation")
	if sc.state != copyPick {
		t.Fatalf("state = %v", sc.state)
	}
	// The selectable one plans, confirms and copies.
	sc.cursor = rowOf("full")
	h.keys("enter")
	if sc.state != copyConfirm {
		t.Fatalf("state = %v view:\n%s", sc.state, h.view())
	}
	wantAll(t, h.view(), "plan: 1 session, 1 memory dir", "copy 1 session to full? [y/N]")
	h.keys("y")
	rs, ok := h.a.top().(*resultScreen)
	if !ok || rs.err != nil {
		t.Fatalf("y should end on a clean result, got %T err=%v:\n%s", h.a.top(), rs.err, h.view())
	}
	wantAll(t, h.view(), "copied 1 session, 1 memory dir", "verify:", "BFFS_ACCOUNT=full", sid1)
	if _, err := os.Stat(filepath.Join(fullProjects, f.slug, sid1+".jsonl")); err != nil {
		t.Errorf("transcript not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fullProjects, f.slug, "memory", "MEMORY.md")); err != nil {
		t.Errorf("memory not copied: %v", err)
	}
}

// The rehome screen shows the old cwd, offers candidates and a typed
// path, renders the dry-run plan and applies it: the transcript moves
// under the new directory's entry with a relocated record appended.
func TestRehomeScreen(t *testing.T) {
	f := newFixture(t)
	old := filepath.Join(f.home, "src", "proj") // the project moved away
	oldSlug, err := transcripts.Slug(old)
	if err != nil {
		t.Fatal(err)
	}
	f.transcript(oldSlug, sid1, old, "prompt from the old place", fixedNow.Add(-time.Hour),
		map[string]any{"type": "assistant", "sessionId": sid1, "message": map[string]any{"role": "assistant", "content": "ok"}})
	h := f.start("sessions")
	wantAll(t, h.view(), "! "+shortPath(old))
	h.keys("3")
	// Nothing selected and nothing pending: r explains.
	h.keys("r")
	wantAll(t, h.view(), "nothing to rehome: select sessions with space")
	if h.a.top() != nil {
		t.Fatalf("r without a subject must stay put, got %T", h.a.top())
	}
	h.keys("space", "r")
	sc, ok := h.a.top().(*rehomeScreen)
	if !ok {
		t.Fatalf("r should open the rehome screen, got %T", h.a.top())
	}
	if sc.state != rehomePick {
		t.Fatalf("state = %v view:\n%s", sc.state, h.view())
	}
	out := h.view()
	wantAll(t, out, "rehome 1 session", "old cwd: "+old, "where does this project live on this machine?", "type a path", "skip this directory")
	// The candidate list may name the directory (same folder name under
	// ~/build); either way the typed path gets there.
	g := sc.groups[0]
	sc.cursor = len(g.candidates) // "type a path"
	h.keys("enter")
	if sc.state != rehomePath {
		t.Fatalf("state = %v", sc.state)
	}
	sc.input.SetValue(filepath.Join(f.home, "nowhere"))
	h.keys("enter")
	wantAll(t, h.view(), "is not a directory here")
	sc.input.SetValue(f.project)
	h.keys("enter")
	if sc.state != rehomeConfirm {
		t.Fatalf("state = %v view:\n%s", sc.state, h.view())
	}
	out = h.view()
	wantAll(t, out, "rehome in", "rule    "+old+" → "+shortPath(f.project),
		"move    "+shortID(sid1), "prompt from the old place", "projects/"+oldSlug+" → projects/"+f.slug,
		"a relocated record is appended", "Rehome 1 session? [y/N]")
	if _, err := os.Stat(filepath.Join(f.claudeDir, "projects", f.slug, sid1+".jsonl")); err == nil {
		t.Fatal("moved before the confirmation")
	}
	h.keys("y")
	rs, ok := h.a.top().(*resultScreen)
	if !ok || rs.err != nil {
		t.Fatalf("y should end on a clean result, got %T err=%v:\n%s", h.a.top(), rs.err, h.view())
	}
	wantAll(t, h.view(), "moved 1 session → projects/"+f.slug, "verify: cd "+f.project+" && claude --resume "+sid1)
	moved := filepath.Join(f.claudeDir, "projects", f.slug, sid1+".jsonl")
	body, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("transcript not moved: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	last := lines[len(lines)-1]
	if len(lines) != 3 || !strings.Contains(last, "relocated") || !strings.Contains(last, f.project) {
		t.Errorf("stamp not appended:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(f.claudeDir, "projects", oldSlug, sid1+".jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old transcript still there: %v", err)
	}
	// The sessions screen underneath lists again after the result closes.
	h.keys("esc")
	if h.a.top() != nil || len(h.a.ws.sel) != 0 {
		t.Fatalf("esc should pop to a refreshed workspace, got %T sel=%v", h.a.top(), h.a.ws.sel)
	}
}

// The trust screen renders the per-account matrix and, on a confirmed
// row, carries the best source's answers into that account's file.
func TestTrustScreen(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.lastSession("work", f.project, sid2)
	if err := store.SaveState(f.cfgDir, store.State{Active: "work"}); err != nil {
		t.Fatal(err)
	}
	key, err := transcripts.ProjectKey(f.project)
	if err != nil {
		t.Fatal(err)
	}
	homeJSON := filepath.Join(f.home, ".claude.json")
	doc := map[string]any{"projects": map[string]any{key: map[string]any{
		"hasTrustDialogAccepted": true, "hasClaudeMdExternalIncludesApproved": true, "hasClaudeMdExternalIncludesWarningShown": true,
	}}}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(homeJSON, b, 0o600); err != nil {
		t.Fatal(err)
	}
	h, _ := f.openSessions()
	h.keys("t")
	sc, ok := h.a.top().(*trustScreen)
	if !ok {
		t.Fatalf("t should open the trust screen, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "project:  "+shortPath(key), "ACCOUNT", "FOLDER-TRUST", "EXTERNAL-IMPORTS", "TOOLS", "MCP",
		"* work", "(home)", "accepted", "allowed", "* = the active account")
	if len(sc.statuses) != 2 || sc.statuses[0].Account != "work" || sc.cursor != 0 {
		t.Fatalf("statuses = %+v cursor = %d", sc.statuses, sc.cursor)
	}
	// Enter on work plans from home; y applies.
	h.keys("enter")
	if sc.state != trustConfirm {
		t.Fatalf("state = %v view:\n%s", sc.state, h.view())
	}
	out = h.view()
	wantAll(t, out, "→ work (from (home))", "hasTrustDialogAccepted", "(absent) -> true", "hasClaudeMdExternalIncludesApproved",
		"will no longer ask the folder-trust dialog or the external CLAUDE.md imports dialog", "apply to")
	h.keys("y")
	wantAll(t, h.view(), "updated work from home (3 fields)", "picks the change up within about a second")
	raw, err := os.ReadFile(filepath.Join(f.cfgDir, "sessions", "work", ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	p := got.Projects[key]
	if p["hasTrustDialogAccepted"] != true || p["hasClaudeMdExternalIncludesApproved"] != true || p["hasClaudeMdExternalIncludesWarningShown"] != true {
		t.Errorf("flags not written: %v", p)
	}
	if p["lastSessionId"] != sid2 {
		t.Errorf("lastSessionId disturbed: %v", p["lastSessionId"])
	}
	// The matrix reloaded: work now answers, and planning again finds
	// nothing to copy.
	out = h.view()
	if strings.Count(out, "accepted") < 2 || strings.Count(out, "allowed") < 2 {
		t.Errorf("matrix not refreshed:\n%s", out)
	}
	h.keys("enter")
	wantAll(t, h.view(), "work is the source of the answers; pick another row")
	// From home's row the plan finds nothing left to copy.
	h.keys("down", "enter")
	wantAll(t, h.view(), "no changes for (home)")
}

// Resume builds the claude child on the account the resolver picks for
// the cwd: CLAUDE_CONFIG_DIR set, the session markers stripped, stdio
// left nil for bubbletea to wire; a live session is refused.
func TestResumeCommand(t *testing.T) {
	f := newFixture(t)
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	stub := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(stub, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BFFS_REAL_CLAUDE", stub)
	t.Setenv("BFFS_NO_USAGE_LOG", "1")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "prompt two", fixedNow.Add(-2*time.Hour))
	f.live(sid2)
	f.lastSession("work", f.project, sid1) // creates the session dir: "logged in"
	if err := store.SaveState(f.cfgDir, store.State{Active: "work"}); err != nil {
		t.Fatal(err)
	}
	svc := f.services()
	root := f.homeRoot(svc)
	sess, err := transcripts.Find([]transcripts.Root{root}, sid1)
	if err != nil || len(sess) != 1 {
		t.Fatalf("Find: %v %d", err, len(sess))
	}
	c, cleanup, err := resumeCommand(svc, sess[0])
	if err != nil {
		t.Fatalf("resumeCommand: %v", err)
	}
	defer cleanup()
	if c.Path != stub || c.Dir != f.project {
		t.Errorf("Path=%q Dir=%q", c.Path, c.Dir)
	}
	if len(c.Args) != 3 || c.Args[1] != "--resume" || c.Args[2] != sid1 {
		t.Errorf("Args = %v", c.Args)
	}
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Errorf("stdio must stay nil for bubbletea: %v %v %v", c.Stdin, c.Stdout, c.Stderr)
	}
	if c.Process != nil {
		t.Error("the process must not be started")
	}
	env := map[string]string{}
	for _, kv := range c.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if want := filepath.Join(f.cfgDir, "sessions", "work"); env["CLAUDE_CONFIG_DIR"] != want {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", env["CLAUDE_CONFIG_DIR"], want)
	}
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		if v, ok := env[k]; ok {
			t.Errorf("session marker %s=%q leaked", k, v)
		}
	}
	if env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Error("deliberate config stripped")
	}

	// The live session is refused, from the function and from the key.
	live, err := transcripts.Find([]transcripts.Root{root}, sid2)
	if err != nil || len(live) != 1 {
		t.Fatalf("Find: %v", err)
	}
	live[0].Live = true
	if _, _, err := resumeCommand(svc, live[0]); err == nil || !strings.Contains(err.Error(), "open in a running claude") {
		t.Errorf("live session: err = %v", err)
	}
	h, s := f.openSessions()
	h.keys("down") // the older, live session
	if r, ok := s.panels[panelItems].list.SelectedItem().(*sessionRow); !ok || r.s.ID != sid2 || !r.s.Live {
		t.Fatalf("cursor row = %+v", s.panels[panelItems].list.SelectedItem())
	}
	h.keys("R")
	wantAll(t, h.view(), "session "+shortID(sid2)+" is open in a running claude; nothing to resume")
	if h.a.top() != nil {
		t.Fatalf("R must stay on the workspace, got %T", h.a.top())
	}
	// A missing cwd is refused too.
	gone := sess[0]
	gone.Cwd = filepath.Join(f.home, "gone")
	if _, _, err := resumeCommand(svc, gone); err == nil || !strings.Contains(err.Error(), "missing on this machine") {
		t.Errorf("missing cwd: err = %v", err)
	}
}

// The receive screen refuses an address off this machine's networks
// before anything is dialled, and its code input echoes nothing.
func TestReceiveScreenHostCheckAndMaskedCode(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	oldLocal, oldDial := fetchLocal, fetchDial
	fetchLocal = func(transfer.LANOptions) ([]transfer.LinkAddr, error) {
		return []transfer.LinkAddr{{Addr: netip.MustParseAddr("192.168.1.20"), Prefix: netip.MustParsePrefix("192.168.1.0/24"), Iface: "en0"}}, nil
	}
	dialled := false
	fetchDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialled = true
		return nil, errors.New("must not dial")
	}
	t.Cleanup(func() { fetchLocal, fetchDial = oldLocal, oldDial })

	h, _ := f.openSessions()
	h.keys("i")
	sc, ok := h.a.top().(*receiveScreen)
	if !ok {
		t.Fatalf("i should open the receive screen, got %T", h.a.top())
	}
	wantAll(t, h.view(), "receive into shared pool: work", "from: ", "only an address on a local network of this machine is dialled")
	for _, bad := range []string{"10.8.0.5", "100.64.1.1", "192.168.2.9", "8.8.8.8"} {
		sc.input.SetValue(bad)
		h.keys("enter")
		wantAll(t, h.view(), "refusing to pair with "+bad, "not on a local network of this machine")
		if sc.state != recvHost || dialled {
			t.Fatalf("%s: state=%v dialled=%v", bad, sc.state, dialled)
		}
	}
	sc.input.SetValue("[fe80::1]")
	h.keys("enter")
	wantAll(t, h.view(), "link-local address fe80::1 needs an interface")
	sc.input.SetValue("nonsense host!")
	h.keys("enter")
	wantAll(t, h.view(), "invalid host")
	if dialled {
		t.Fatal("a refused target was dialled")
	}
	// esc leaves the screen.
	h.keys("esc")
	if h.a.top() != nil {
		t.Fatalf("esc should pop, got %T", h.a.top())
	}

	// The code field: what is typed never shows, a bad code is named, a
	// good one is handed to the fetch.
	svc := f.services()
	rs := newReceiveScreen(svc, f.homeRoot(svc))
	_, _ = rs.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	rs.state = recvCode
	rs.codeCh = make(chan transfer.Code, 1)
	rs.op = &op{ctx: context.Background(), cancel: func() {}, ch: make(chan tea.Msg)}
	wantAll(t, rs.View(120, 30), "pairing code: ", "nothing is echoed", "XXXX-XXXX as shown there")
	for _, r := range "7k3q-m9x" {
		_, _ = rs.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	out := rs.View(120, 30)
	wantAll(t, out, "pairing code: ")
	wantNone(t, out, "7k3q", "m9x", "7K3Q")
	_, _ = rs.Update(keyPress("enter"))
	wantAll(t, rs.View(120, 30), "invalid pairing code")
	if len(rs.codeCh) != 0 {
		t.Fatal("a bad code was sent")
	}
	for _, r := range "7k3q-m9xd" {
		_, _ = rs.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	wantNone(t, rs.View(120, 30), "7k3q", "m9xd")
	_, _ = rs.Update(keyPress("enter"))
	select {
	case c := <-rs.codeCh:
		if c.Display() != "7K3Q-M9XD" {
			t.Errorf("code = %q", c.Display())
		}
	default:
		t.Fatal("the code was not handed to the fetch")
	}
	wantNone(t, rs.View(120, 30), "7K3Q", "m9xd")
	if rs.state != recvAuth {
		t.Errorf("state = %v", rs.state)
	}
}

// The serve screen renders the banner with the code exactly once — never
// in a log line or the result — and delivers the bundle to a loopback
// receiver that knows the code.
func TestServeScreenLoopback(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	code := mustParseCode(t, "7K3Q-M9XD")
	addrCh := loopbackTransfer(t, code)
	svc := f.services()
	root := f.homeRoot(svc)
	tgt := actionTarget{root: root, slug: f.slug, project: f.project}

	sc, err := newServeScreen(svc, tgt)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sc.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	wantAll(t, sc.View(160, 40), "send the whole project", "hashing")
	if nav := drive(t, sc, sc.Init()()); nav != nil {
		t.Fatalf("prepare navigated: %#v", nav)
	}
	if sc.state != serveConfirm {
		t.Fatalf("state = %v", sc.state)
	}
	out := sc.View(160, 40)
	wantAll(t, out, "Exporting from the shared pool", "1 session", "memory        2 files", "Serve this over the local network?", "[y/N]")
	wantNone(t, out, "7K3Q")

	// Machine B, once the serve is bound.
	var received int64
	staging := t.TempDir()
	fetched := make(chan error, 1)
	go func() {
		var addr netip.AddrPort
		select {
		case addr = <-addrCh:
		case <-time.After(10 * time.Second):
			fetched <- errors.New("serve never bound")
			return
		}
		_, err := transfer.Fetch(context.Background(), transfer.FetchOptions{
			Addr:    addr,
			Code:    func() (transfer.Code, error) { return code, nil },
			Confirm: func([]byte) (bool, error) { return true, nil },
			Sink: func(ctx context.Context, manifest []byte, body io.Reader) (transfer.Done, error) {
				// The connection stays open for B's report, so the archive
				// is read to its own end, the way porter.Import does.
				u, err := bundle.Unpack(ctx, body, staging, "", bundle.DefaultLimits, false, nil)
				if err != nil {
					return transfer.Done{Reason: err.Error()}, err
				}
				received = u.Manifest.Totals.Bytes
				return transfer.Done{OK: true, Entries: len(u.Files), Bytes: u.Manifest.Totals.Bytes}, nil
			},
			LAN:   transfer.LANOptions{AllowLoopback: true},
			Local: loopbackLinks(),
			Host:  "mac-b",
			User:  "jonas",
		})
		fetched <- err
	}()

	// y starts the serve; the messages are fed one by one so the banner
	// can be inspected before the transfer completes.
	_, cmd := sc.Update(keyPress("y"))
	if sc.state != serveServing || cmd == nil {
		t.Fatalf("state = %v", sc.state)
	}
	var (
		pending    = []tea.Cmd{cmd}
		bannerSeen bool
		result     Screen
		views      []string
	)
	deadline := time.Now().Add(20 * time.Second)
	for len(pending) > 0 && result == nil {
		if time.Now().After(deadline) {
			t.Fatal("serve did not finish")
		}
		c := pending[0]
		pending = pending[1:]
		msg := c()
		switch m := msg.(type) {
		case nil:
			continue
		case tea.BatchMsg:
			pending = append(pending, m...)
			continue
		case tickMsg:
			continue
		case replaceScreenMsg:
			result = m.screen
			continue
		case popScreenMsg, statusMsg:
			t.Fatalf("serve ended with %#v; views:\n%s", m, strings.Join(views, "\n----\n"))
		}
		_, next := sc.Update(msg)
		if next != nil {
			pending = append(pending, next)
		}
		v := sc.View(160, 40)
		views = append(views, v)
		if sc.bound && !bannerSeen {
			bannerSeen = true
			wantAll(t, v, "On the other machine, run:    bffs import --from 127.0.0.1:", "7K3Q-M9XD", "this machine's key: ",
				"Waiting for the other machine…  code valid for ", "3 attempts, one transfer", "(esc cancels)")
			if strings.Contains(v, "this machine's key: unknown") {
				t.Errorf("key fingerprint missing:\n%s", v)
			}
		}
	}
	if !bannerSeen {
		t.Fatal("the banner was never rendered")
	}
	for _, v := range views {
		if n := strings.Count(v, "7K3Q-M9XD"); n != 1 {
			t.Errorf("code rendered %d times in one frame:\n%s", n, v)
		}
	}
	if err := <-fetched; err != nil {
		t.Fatalf("fetch: %v", err)
	}
	rs, ok := result.(*resultScreen)
	if !ok {
		t.Fatalf("serve should end on the result screen, got %T", result)
	}
	if rs.err != nil {
		t.Fatalf("serve failed: %v", rs.err)
	}
	_, _ = rs.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	out = rs.View(160, 40)
	wantAll(t, out, "delivered: 3 files", "verified by mac-b (jonas)", "Done.")
	wantNone(t, out, "7K3Q")
	if received == 0 {
		t.Error("nothing received")
	}
	// The event log named the peer and the review, never the code.
	joined := strings.Join(sc.events, "\n")
	wantAll(t, joined, "127.0.0.1 connected — waiting for its code", "code accepted; manifest sent", "manifest accepted by mac-b")
	wantNone(t, joined, "7K3Q")
}

// While an operation runs the app hands the screen every key: q asks
// before quitting (n stays, y cancels and quits), esc and ctrl+c cancel
// the context, and the screen unwinds when the goroutine reports.
func TestOpInFlightQuitConfirmAndCancel(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h, s := f.openSessions()

	blocking := func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
		<-ctx.Done()
		return exportPreparedMsg{err: ctx.Err()}
	}
	sc := newExportScreen(h.a.svc, s.target())
	h.send(pushScreenMsg{screen: sc})
	sc.state = exportPreparing
	sc.op, _ = startOp(h.a.svc.ctx, blocking) // the wait command is run by hand below
	if !h.a.busy() {
		t.Fatal("app should be busy while the op runs")
	}
	// Navigation is blocked: esc does not pop, q does not quit.
	h.keys("q")
	wantAll(t, h.view(), quitPrompt)
	if h.quit || h.a.top() != sc {
		t.Fatalf("q must ask, not quit: quit=%v top=%T", h.quit, h.a.top())
	}
	h.keys("n")
	wantNone(t, h.view(), quitPrompt)
	if sc.op.cancelled() {
		t.Fatal("n must not cancel")
	}
	h.keys("esc")
	if !sc.op.cancelled() {
		t.Fatal("esc must cancel the operation's context")
	}
	wantAll(t, h.view(), cancelling)
	if h.a.top() != sc {
		t.Fatalf("esc must not pop while the op unwinds, got %T", h.a.top())
	}
	// The goroutine reports; the screen pops with a status line.
	h.run(sc.op.wait())
	if h.a.top() != nil {
		t.Fatalf("after the cancelled op the workspace should be back, got %T", h.a.top())
	}
	wantAll(t, h.view(), "export cancelled; nothing written")
	if h.a.busy() {
		t.Fatal("app still busy")
	}

	// ctrl+c while busy cancels rather than quits.
	sc = newExportScreen(h.a.svc, s.target())
	h.send(pushScreenMsg{screen: sc})
	sc.state = exportPreparing
	sc.op, _ = startOp(h.a.svc.ctx, blocking)
	h.keys("ctrl+c")
	if h.quit || !sc.op.cancelled() {
		t.Fatalf("ctrl+c while busy: quit=%v cancelled=%v", h.quit, sc.op.cancelled())
	}
	h.run(sc.op.wait())

	// q then y cancels and quits.
	sc = newExportScreen(h.a.svc, s.target())
	h.send(pushScreenMsg{screen: sc})
	sc.state = exportPreparing
	sc.op, _ = startOp(h.a.svc.ctx, blocking)
	h.keys("q", "y")
	if !h.quit || !sc.op.cancelled() {
		t.Fatalf("q y: quit=%v cancelled=%v", h.quit, sc.op.cancelled())
	}
	// The quit waited for the goroutine: its channel is already closed.
	if _, open := <-sc.op.ch; open {
		t.Error("the confirmed quit should drain the op before ending")
	}
}

// The action keys are bound where they have a subject and answer with
// the hint elsewhere; i (receive) works from every screen.
func TestActionKeyPlacement(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	h := f.start("memories")
	ws := h.a.ws
	if ws.focus != panelItems || ws.tab != tabMemory {
		t.Fatalf("bffs memory should open on the memory tab: focus=%d tab=%d", ws.focus, ws.tab)
	}
	// Memory tab: no rehome, resume or pointer; the hint says where.
	h.keys("R")
	wantAll(t, h.view(), "select a session on the sessions tab")
	h.keys("r")
	wantAll(t, h.view(), "nothing to rehome")
	h.keys("d")
	wantAll(t, h.view(), "never deletes")
	// Roots panel: i opens receive into the root.
	h.keys("1", "i")
	if _, ok := h.a.top().(*receiveScreen); !ok {
		t.Fatalf("i on roots should open receive, got %T", h.a.top())
	}
	h.keys("esc")
	// Memory tab: export, copy, trust and the memory sync are there.
	h.keys("3")
	h.keys("t")
	if _, ok := h.a.top().(*trustScreen); !ok {
		t.Fatalf("t on memory should open trust, got %T", h.a.top())
	}
	wantAll(t, h.view(), "FOLDER-TRUST")
	h.keys("esc")
	h.keys("e")
	if _, ok := h.a.top().(*exportScreen); !ok {
		t.Fatalf("e on memory should open export, got %T", h.a.top())
	}
	wantAll(t, h.view(), "export the whole project")
	h.keys("esc")
	h.keys("c")
	if _, ok := h.a.top().(*copyScreen); !ok {
		t.Fatalf("c on memory should open copy, got %T", h.a.top())
	}
	h.keys("esc")
	h.keys("S")
	if sc, ok := h.a.top().(*copyScreen); !ok || sc.tgt.only != "memories" {
		t.Fatalf("S should open the memory-only copy, got %T", h.a.top())
	}
	wantAll(t, h.view(), "copy the memory of "+shortPath(f.project))
	h.keys("esc")
	// Serve refuses at once when there is no local network.
	old := serveLocal
	serveLocal = func(transfer.LANOptions) ([]transfer.LinkAddr, error) { return nil, nil }
	t.Cleanup(func() { serveLocal = old })
	h.keys("s")
	wantAll(t, h.view(), "no local-network address found")
	if h.a.top() != nil {
		t.Fatalf("s without a network must stay put, got %T", h.a.top())
	}
}

// A file that appears under the output path between the confirmation
// and the rename is never replaced: the write fails, the file is intact
// and no temporary is left beside it.
func TestExportNeverOverwritesFileThatAppeared(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	svc := f.services()
	root := f.homeRoot(svc)
	tgt := actionTarget{root: root, slug: f.slug, project: f.project}
	sel, _, _, err := tgt.selection(context.Background(), svc)
	if err != nil {
		t.Fatal(err)
	}
	opts := exportOptions(svc, root, nil)
	m, opener, _, err := porter.BuildManifest(context.Background(), sel, opts)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "late.bffs")
	if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBundleFile(context.Background(), path, m, opener, opts); err == nil || !strings.Contains(err.Error(), "appeared while the bundle was being written") {
		t.Fatalf("writeBundleFile over an existing file: err = %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "theirs" {
		t.Errorf("the existing file was touched: %q %v", b, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temporary left behind: %v", entries)
	}
}

// The receive screen's keys off the happy path: esc while the host is
// still being checked leaves the screen; on the confirmation ctrl+c
// cancels the fetch and q asks before quitting; the confirmed quit waits
// for the goroutine to unwind.
func TestReceiveKeysWhileResolvingAndConfirming(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h, _ := f.openSessions()
	h.keys("i")
	sc, ok := h.a.top().(*receiveScreen)
	if !ok {
		t.Fatalf("i should open the receive screen, got %T", h.a.top())
	}
	sc.state = recvResolving // a slow name lookup is in flight; nothing runs yet
	h.keys("esc")
	if h.a.top() != nil {
		t.Fatalf("esc while resolving should pop, got %T", h.a.top())
	}

	blocking := func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
		<-ctx.Done()
		return recvDoneMsg{err: ctx.Err()}
	}
	svc := f.services()
	rs := newReceiveScreen(svc, f.homeRoot(svc))
	rs.state = recvConfirm
	rs.answerCh = make(chan recvAnswer, 1)
	rs.op, _ = startOp(context.Background(), blocking)
	_, _ = rs.Update(keyPress("q"))
	if !rs.askQuit || rs.op.cancelled() {
		t.Fatalf("q on the confirmation: askQuit=%v cancelled=%v", rs.askQuit, rs.op.cancelled())
	}
	_, _ = rs.Update(keyPress("n"))
	_, _ = rs.Update(keyPress("ctrl+c"))
	if !rs.op.cancelled() {
		t.Fatal("ctrl+c on the confirmation must cancel the fetch")
	}
	if len(rs.answerCh) != 0 {
		t.Fatal("ctrl+c must not answer the confirmation")
	}
	_ = rs.op.wait()() // the goroutine reports

	// q then y: the quit command returns only once the op has unwound.
	rs.op, _ = startOp(context.Background(), blocking)
	rs.askQuit = false
	_, _ = rs.Update(keyPress("q"))
	_, cmd := rs.Update(keyPress("y"))
	if cmd == nil {
		t.Fatal("y should return the quit command")
	}
	if _, ok := cmd().(quitMsg); !ok {
		t.Fatal("the confirmed quit should end in quitMsg")
	}
	if _, open := <-rs.op.ch; open {
		t.Error("the op channel should be drained and closed before quitting")
	}
}
