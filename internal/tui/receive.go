package tui

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

// fetchLookup resolves a name once; the loopback tests replace it.
var fetchLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// recvState is where the Receive screen is.
type recvState int

const (
	recvHost       recvState = iota // typing host[:port]
	recvResolving                   // name lookup + on-link check
	recvConnecting                  // transfer.Fetch dials
	recvCode                        // the masked code input
	recvAuth                        // proofs exchanged, the manifest is on its way
	recvPlace                       // where does this project live?
	recvPath                        // typing a path for it
	recvConfirm                     // summary + placements + [y/N]
	recvImporting                   // porter.Import streams the body
	recvPeeking                     // a file source: reading its manifest
)

// recvProject is one project of the incoming bundle and how it will be
// placed.
type recvProject struct {
	label       string
	cwd         string
	slug        string
	exists      bool
	mapped      string // the local directory chosen; "" = as-is
	decided     bool   // the user answered for it (mapped, or as-is)
	sessions    int
	bytes       int64
	memoryFiles int
	trust       string
	candidates  []rehome.Candidate
}

// recvResolvedMsg ends the host check.
type recvResolvedMsg struct {
	addr netip.AddrPort
	warn string
	err  error
}

// recvManifestMsg is the manifest the other side sent, reviewed.
type recvManifestMsg struct {
	header   string
	projects []recvProject
	limit    int64
	days     int
	source   string
}

// recvAnswer is what the screen tells the Confirm callback.
type recvAnswer struct {
	rules []rehome.Mapping
	ok    bool
}

// recvPeekedMsg ends the manifest read of a file source.
type recvPeekedMsg struct {
	path   string
	review recvManifestMsg
	digest string
	err    error
}

// recvDoneMsg ends the fetch.
type recvDoneMsg struct {
	rep     porter.Report
	err     error
	sinkRan bool
	sinkErr error
	elapsed time.Duration
}

// receiveScreen is `bffs import --from <host>` into the root of the
// screen that pushed it: host → the same on-link check the CLI makes
// (nothing is dialled otherwise) → connect → a masked code input → the
// manifest summary with a placement per project (identity when the
// directory exists here, else rehome.Suggest's candidates, a typed path
// or as-is) → [y/N] → porter.Import in stream mode with the manifest
// sha256 pinned → progress → the receipt.
type receiveScreen struct {
	svc      *services
	root     transcripts.Root
	account  string
	state    recvState
	input    textinput.Model
	code     textinput.Model
	note     string
	addr     netip.AddrPort
	op       *op
	prog     opView
	codeCh   chan transfer.Code
	answerCh chan recvAnswer
	events   []string
	peerKey  string
	header   string
	projects []recvProject
	limit    int64
	days     int
	daysSrc  string
	current  int // the project being placed
	cursor   int
	askQuit  bool
	width    int
	file     string // a .bffs file source instead of a host
	digest   string // the reviewed manifest's sha256 (file source)
	box      scrollBox
}

// newReceiveFileScreen is `bffs import --from <file.bffs>` into root:
// the manifest is read and reviewed first, placement and confirmation
// are the receive screen's, then porter.Import reads the file again
// with the reviewed manifest's sha256 pinned.
func newReceiveFileScreen(svc *services, root transcripts.Root, account, path string) *receiveScreen {
	s := newReceiveScreen(svc, root, account)
	s.file, s.state = path, recvPeeking
	s.prog = newOpView("importing")
	return s
}

// peekFile reads a bundle file's manifest for review.
func peekFile(path string, root transcripts.Root, account string) tea.Cmd {
	return func() tea.Msg {
		f, err := os.Open(path)
		if err != nil {
			return recvPeekedMsg{path: path, err: err}
		}
		defer f.Close()
		m, raw, _, err := bundle.PeekManifest(bufio.NewReaderSize(f, 256<<10))
		if err != nil {
			return recvPeekedMsg{path: path, err: fmt.Errorf("bundle: %w", err)}
		}
		limits := bundle.DefaultLimits
		if err := m.Validate(limits); err != nil {
			return recvPeekedMsg{path: path, err: fmt.Errorf("bundle: %w", err)}
		}
		sum := sha256.Sum256(raw)
		return recvPeekedMsg{path: path, review: reviewManifest(m, root, account, limits.MaxTotalBytes), digest: hex.EncodeToString(sum[:])}
	}
}

// newReceiveScreen receives into root as account (the perspective the
// browser has selected; "" = the resolver's pick for the cwd, the CLI's
// default without --account).
func newReceiveScreen(svc *services, root transcripts.Root, account string) *receiveScreen {
	if account == "" || account == transcripts.HomeName {
		account = destAccount(svc, root)
	}
	code := newInput("pairing code: ", "the 8 or 12 characters shown on the other machine (nothing is echoed)")
	code.EchoMode = textinput.EchoNone
	return &receiveScreen{
		svc: svc, root: root, account: account,
		input: newInput("from: ", "IPv4 address, or host[:port], shown on the other machine"),
		code:  code,
		prog:  newOpView("receiving"),
	}
}

func (s *receiveScreen) Init() tea.Cmd {
	if s.file != "" {
		return peekFile(s.file, s.root, s.account)
	}
	return nil
}

func (s *receiveScreen) Title() string {
	if s.file != "" {
		return "import file"
	}
	return "receive"
}
func (s *receiveScreen) running() bool { return s.op.active() }
func (s *receiveScreen) capturingInput() bool {
	return s.state == recvHost || s.state == recvCode || s.state == recvPath
}

func (s *receiveScreen) Keys() []key.Binding {
	switch s.state {
	case recvHost:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "connect")), keys.Cancel}
	case recvCode, recvPath:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")), keys.Cancel}
	case recvPlace:
		return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")), keys.Cancel}
	case recvConfirm:
		return append([]key.Binding{keys.Yes, keys.No}, scrollKeys()...)
	}
	return []key.Binding{keys.Cancel}
}

// splitFromHost separates a target into host and port: "[v6]", "[v6]:p",
// "v6" (two or more colons), "v4", "v4:p", "name", "name:p". The port
// defaults to the serve default.
func splitFromHost(s string) (string, uint16, error) {
	s = strings.TrimSpace(s)
	host, portS := s, ""
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", 0, fmt.Errorf("invalid address %q: missing ]", s)
		}
		host = s[1:end]
		rest := s[end+1:]
		switch {
		case rest == "":
		case strings.HasPrefix(rest, ":"):
			portS = rest[1:]
		default:
			return "", 0, fmt.Errorf("invalid address %q: use [address]:port", s)
		}
	case strings.Count(s, ":") >= 2:
	case strings.Contains(s, ":"):
		host, portS, _ = strings.Cut(s, ":")
	}
	if host == "" {
		return "", 0, errors.New("type the address shown on the other machine")
	}
	port := uint16(servePort)
	if portS != "" {
		n, err := strconv.Atoi(portS)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("invalid port %q: use 1-65535", portS)
		}
		port = uint16(n)
	}
	return host, port, nil
}

// validHostname accepts DNS-shaped names.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, "-") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// resolveHost turns what was typed into the literal to dial (plan §8.6):
// an address with an optional port, or a name resolved once. Every
// address must lie on a local network of this machine (transfer.IsLAN
// against the local set; the denylist always applies) before anything is
// dialled. Nothing is dialled here.
func resolveHost(ctx context.Context, target string, local []transfer.LinkAddr, lan transfer.LANOptions) (netip.AddrPort, string, error) {
	host, port, err := splitFromHost(target)
	if err != nil {
		return netip.AddrPort{}, "", err
	}
	refuse := func(ip netip.Addr) error {
		return fmt.Errorf("refusing to pair with %s: %w. Export to a file instead (e)", ip.WithZone(""), transfer.ErrNotLAN)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Is6() && ip.IsLinkLocalUnicast() && ip.Zone() == "" {
			return netip.AddrPort{}, "", fmt.Errorf("link-local address %s needs an interface: [%s%%<iface>]", ip, ip)
		}
		if !transfer.IsLAN(ip, local, lan) {
			return netip.AddrPort{}, "", refuse(ip)
		}
		return netip.AddrPortFrom(ip, port), "", nil
	}
	if !validHostname(host) {
		return netip.AddrPort{}, "", fmt.Errorf("invalid host %q: use the address shown on the other machine", host)
	}
	warn := ""
	if strings.HasSuffix(strings.ToLower(host), ".local") || !strings.Contains(host, ".") {
		warn = "any host on this network can answer that name — the IPv4 address shown on the other machine is the safe form"
	}
	addrs, err := fetchLookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return netip.AddrPort{}, "", fmt.Errorf("could not resolve %q: use the IP address shown on the other machine", host)
	}
	var pick netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if !transfer.IsLAN(a, local, lan) {
			return netip.AddrPort{}, "", refuse(a)
		}
		if !pick.IsValid() || (a.Is4() && !pick.Is4()) {
			pick = a
		}
	}
	return netip.AddrPortFrom(pick, port), warn, nil
}

// resolve is the command behind enter on the host input.
func resolve(ctx context.Context, target string) tea.Cmd {
	return func() tea.Msg {
		lan := lanOptions
		local, err := fetchLocal(lan)
		if err != nil {
			return recvResolvedMsg{err: err}
		}
		addr, warn, err := resolveHost(ctx, target, local, lan)
		return recvResolvedMsg{addr: addr, warn: warn, err: err}
	}
}

// reviewManifest turns the peer's manifest into the summary the screen
// shows: one project per cwd/slug with the identity rule applied, the
// candidates rehome.Suggest proposes for the rest, the retention line.
func reviewManifest(m *bundle.Manifest, root transcripts.Root, account string, limit int64) recvManifestMsg {
	src := m.Source
	acct := transcripts.Sanitize(src.Account)
	if acct == "" {
		acct = transcripts.HomeName
	}
	header := fmt.Sprintf("%s from %s (%s, %s/%s, bffs %s, claude %s, account %q, %s)", short8(m.BundleID),
		transcripts.Sanitize(src.Hostname), transcripts.Sanitize(src.User), transcripts.Sanitize(src.OS), transcripts.Sanitize(src.Arch),
		transcripts.Sanitize(m.BFFSVersion), transcripts.Sanitize(m.ClaudeVersion), acct, transcripts.Sanitize(src.Isolation))
	var projects []recvProject
	index := map[string]int{}
	group := func(key string) *recvProject {
		if i, ok := index[key]; ok {
			return &projects[i]
		}
		index[key] = len(projects)
		projects = append(projects, recvProject{})
		return &projects[len(projects)-1]
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		key := e.Cwd
		if key == "" {
			key = "slug:" + e.Slug
		}
		p := group(key)
		if p.label == "" {
			p.cwd, p.slug = e.Cwd, e.Slug
			p.label = transcripts.Sanitize(e.Cwd)
			if e.Cwd == "" {
				p.label = "projects/" + transcripts.Sanitize(e.Slug) + " (no cwd recorded)"
			} else if isDir(e.Cwd) {
				p.exists = true
			}
		}
		switch e.Kind {
		case bundle.EntrySession:
			p.sessions++
			for _, f := range e.Files {
				p.bytes += f.Size
			}
			if e.SourceTrust != nil && p.trust == "" {
				p.trust = "not accepted"
				if e.SourceTrust.Accepted {
					p.trust = "accepted"
				}
			}
		case bundle.EntryMemory:
			p.memoryFiles += len(e.Files)
		}
	}
	rec := imports.Record{BundleID: m.BundleID, Source: imports.Source{Hostname: src.Hostname, User: src.User, Home: src.Home, OS: src.OS, Account: src.Account}}
	for _, e := range m.Entries {
		switch e.Kind {
		case bundle.EntrySession:
			rec.Sessions = append(rec.Sessions, imports.Session{ID: e.SessionID, OldCwd: e.Cwd, OldSlug: e.Slug, Title: e.Title, GitRemote: e.GitRemote})
		case bundle.EntryMemory:
			rec.Memories = append(rec.Memories, imports.Memory{OldCwd: e.Cwd})
		}
	}
	home, _ := os.UserHomeDir()
	for _, sg := range rehome.Suggest(rec, home, nil) {
		if i, ok := index[sg.OldCwd]; ok {
			projects[i].candidates = sg.Candidates
		}
	}
	days, source := transcripts.CleanupPeriodDays(root.ConfigDir)
	return recvManifestMsg{header: header, projects: projects, limit: limit, days: days, source: source}
}

// start dials: transfer.Fetch runs in the op goroutine, asks the screen
// for the code and the answer through channels, and streams the body
// into porter.Import.
func (s *receiveScreen) start() (Screen, tea.Cmd) {
	s.state = recvConnecting
	s.codeCh = make(chan transfer.Code, 1)
	s.answerCh = make(chan recvAnswer, 1)
	svc, root, account, addr := s.svc, s.root, s.account, s.addr
	codeCh, answerCh := s.codeCh, s.answerCh
	limits := bundle.DefaultLimits
	var cmd tea.Cmd
	s.op, cmd = startOp(s.svc.ctx, func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
		var (
			digest  string
			out     recvDoneMsg
			rules   []rehome.Mapping
			started time.Time
		)
		lan := lanOptions
		local, _ := fetchLocal(lan)
		confirm := func(raw []byte) (bool, error) {
			var m bundle.Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return false, fmt.Errorf("manifest from %s: %w", addr, err)
			}
			if err := m.Validate(limits); err != nil {
				return false, fmt.Errorf("bundle: %w", err)
			}
			sum := sha256.Sum256(raw)
			digest = hex.EncodeToString(sum[:])
			emit(reviewManifest(&m, root, account, limits.MaxTotalBytes))
			select {
			case a := <-answerCh:
				rules = a.rules
				return a.ok, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		sink := func(ctx context.Context, raw []byte, body io.Reader) (transfer.Done, error) {
			out.sinkRan = true
			live, err := transcripts.Live(ctx, svc.configDirs())
			if err != nil {
				live = nil
			}
			opts := porter.ImportOptions{
				Dest:                 root,
				Account:              account,
				OnConflict:           porter.ConflictSkip,
				Limits:               limits,
				ExpectManifestSHA256: digest,
				Now:                  svc.now(),
				Progress:             func(p bundle.Progress) { emit(progressMsg{p: p}) },
				Live:                 live,
				LaunchEnv:            os.Environ(),
				StreamMode:           true,
				Map:                  rules,
			}
			started = time.Now()
			out.rep, out.sinkErr = porter.Import(ctx, svc.cfgDir, bufio.NewReaderSize(body, 256<<10), opts)
			out.elapsed = time.Since(started)
			d := transfer.Done{Entries: len(out.rep.Imported) + len(out.rep.Pending), Bytes: out.rep.Bytes}
			if out.sinkErr != nil {
				d.Reason = out.sinkErr.Error()
			}
			return d, out.sinkErr
		}
		_, err := transfer.Fetch(ctx, transfer.FetchOptions{
			Addr: addr,
			Code: func() (transfer.Code, error) {
				select {
				case c := <-codeCh:
					return c, nil
				case <-ctx.Done():
					return transfer.Code{}, ctx.Err()
				}
			},
			Confirm: confirm,
			Sink:    sink,
			Events:  func(ev transfer.Event) { emit(transferEventMsg{ev: ev}) },
			LAN:     lan,
			Dial:    fetchDial,
			Version: svc.version,
			Host:    hostIdent(),
			User:    userIdent(),
			Local:   local,
		})
		out.err = err
		return out
	})
	return s, cmd
}

// undecided is the index of the next project without an answer from i
// on, or -1. A project whose directory exists here needs none (identity),
// nor does one without a recorded cwd (as-is is all it can be).
func (s *receiveScreen) undecided(from int) int {
	for i := from; i < len(s.projects); i++ {
		p := s.projects[i]
		if !p.exists && p.cwd != "" && !p.decided {
			return i
		}
	}
	return -1
}

// options are the placement choices of the current project.
func (s *receiveScreen) options() []string {
	p := s.projects[s.current]
	out := make([]string, 0, len(p.candidates)+2)
	for _, c := range p.candidates {
		out = append(out, fmt.Sprintf("%-44s (%s)", shortPath(c.Dir), transcripts.Sanitize(c.Reason)))
	}
	return append(out, "type a path", "import as-is; rehome later (r)")
}

// place records the answer for the current project ("" = as-is) and
// moves to the next undecided one, or to the confirmation.
func (s *receiveScreen) place(dir string) {
	p := &s.projects[s.current]
	p.mapped, p.decided = dir, true
	s.cursor = 0
	if next := s.undecided(s.current + 1); next >= 0 {
		s.current = next
		s.state = recvPlace
		return
	}
	s.state = recvConfirm
}

// rules are the mappings the answers made — a mapping is a confirmation.
func (s *receiveScreen) rules() []rehome.Mapping {
	var out []rehome.Mapping
	for _, p := range s.projects {
		if p.mapped != "" && p.cwd != "" {
			out = append(out, rehome.Mapping{Old: p.cwd, New: p.mapped})
		}
	}
	return out
}

func (s *receiveScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.input.SetWidth(max(10, msg.Width-8))
		s.code.SetWidth(max(10, msg.Width-16))
		return s, nil

	case recvResolvedMsg:
		if s.state != recvResolving {
			return s, nil
		}
		if msg.err != nil {
			s.state = recvHost
			s.note = msg.err.Error()
			return s, nil
		}
		s.addr = msg.addr
		s.note = ""
		if msg.warn != "" {
			s.events = append(s.events, "warning: "+msg.warn)
		}
		return s.start()

	case progressMsg:
		s.prog.update(msg.p)
		return s, s.op.wait()

	case transferEventMsg:
		ev := msg.ev
		switch ev.Kind {
		case "connect":
			s.peerKey = keyFromText(ev.Text, "peer key ")
			s.events = append(s.events, fmt.Sprintf("connected to %s (TLS 1.3, peer key %s) — it asks for the pairing code", fromTarget(s.addr.Addr(), s.addr.Port()), s.peerKey))
			if s.state == recvConnecting {
				s.state = recvCode
			}
		case "code-ok":
			s.events = append(s.events, "code accepted — the other machine proved it knows the code too")
			if s.state == recvCode {
				s.state = recvAuth
			}
		case "bad-code", "reject", "error":
			s.events = append(s.events, transcripts.Sanitize(ev.Text))
		}
		s.prog.turn()
		return s, s.op.wait()

	case recvManifestMsg:
		s.review(msg)
		return s, s.op.wait()

	case recvPeekedMsg:
		if msg.path != s.file || s.state != recvPeeking {
			return s, nil
		}
		if msg.err != nil {
			return s, tea.Batch(popScreen(), statusError(msg.err))
		}
		s.digest = msg.digest
		s.review(msg.review)
		return s, nil

	case recvDoneMsg:
		s.op.finish()
		return s.finish(msg)

	case tea.KeyPressMsg:
		return s.keyPress(msg)
	}
	switch s.state {
	case recvHost, recvPath:
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	case recvCode:
		var cmd tea.Cmd
		s.code, cmd = s.code.Update(msg)
		return s, cmd
	}
	return s, nil
}

// review takes the manifest summary in and moves to the first placement
// question or straight to the confirmation.
func (s *receiveScreen) review(msg recvManifestMsg) {
	s.header, s.projects, s.limit, s.days, s.daysSrc = msg.header, msg.projects, msg.limit, msg.days, msg.source
	s.cursor = 0
	if i := s.undecided(0); i >= 0 {
		s.current = i
		s.state = recvPlace
	} else {
		s.state = recvConfirm
	}
}

// startFile imports the reviewed file: porter.Import reads it again
// from the start with the reviewed manifest's sha256 pinned, so a file
// swapped meanwhile is refused.
func (s *receiveScreen) startFile() (Screen, tea.Cmd) {
	s.state = recvImporting
	svc, root, account, path, digest, rules := s.svc, s.root, s.account, s.file, s.digest, s.rules()
	var cmd tea.Cmd
	s.op, cmd = startOp(s.svc.ctx, func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
		out := recvDoneMsg{sinkRan: true}
		f, err := os.Open(path)
		if err != nil {
			out.err, out.sinkErr = err, err
			return out
		}
		defer f.Close()
		live, err := transcripts.Live(ctx, svc.configDirs())
		if err != nil {
			live = nil
		}
		opts := porter.ImportOptions{
			Dest:                 root,
			Account:              account,
			OnConflict:           porter.ConflictSkip,
			Limits:               bundle.DefaultLimits,
			ExpectManifestSHA256: digest,
			Now:                  svc.now(),
			Progress:             func(p bundle.Progress) { emit(progressMsg{p: p}) },
			Live:                 live,
			LaunchEnv:            os.Environ(),
			Map:                  rules,
		}
		started := time.Now()
		out.rep, out.sinkErr = porter.Import(ctx, svc.cfgDir, bufio.NewReaderSize(f, 256<<10), opts)
		out.elapsed = time.Since(started)
		out.err = out.sinkErr
		return out
	})
	return s, cmd
}

// finish maps the fetch's outcome to a result screen or a status line,
// the exit paths of the CLI's runImportFromHost.
func (s *receiveScreen) finish(msg recvDoneMsg) (Screen, tea.Cmd) {
	err := msg.err
	lines := importReceiptLines(s.svc.cfgDir, msg.rep, s.account)
	landed := len(msg.rep.Imported) + len(msg.rep.Pending) + len(msg.rep.MemoryDirs)
	switch {
	case err == nil:
		return s, replaceScreen(newResultScreen("receive", append([]string{fmt.Sprintf("Done in %.1fs.", msg.elapsed.Seconds()), ""}, lines...), nil))
	case errors.Is(err, transfer.ErrRejected):
		return s, tea.Batch(popScreen(), status("import cancelled; nothing written"))
	case s.op.cancelled() || errors.Is(err, context.Canceled):
		if landed == 0 {
			return s, tea.Batch(popScreen(), status("cancelled; the connection was closed and nothing was written"))
		}
		return s, replaceScreen(newResultScreen("receive", append([]string{"import interrupted; what landed before:"}, lines...), nil))
	case errors.Is(err, transfer.ErrBadCode):
		return s, replaceScreen(newResultScreen("receive", s.events, err))
	case msg.sinkErr != nil:
		if landed > 0 {
			lines = append([]string{"import stopped; what landed before the failure:"}, lines...)
		} else {
			lines = nil
		}
		return s, replaceScreen(newResultScreen("receive", lines, err))
	case msg.sinkRan:
		// Everything landed; only the completion report to the other
		// machine failed (it went away first). The import is whole.
		lines = append(lines, "", fmt.Sprintf("warning: %v — the import itself is complete; the other machine may show it as unfinished", err))
		return s, replaceScreen(newResultScreen("receive", lines, nil))
	default:
		return s, replaceScreen(newResultScreen("receive", s.events, err))
	}
}

func (s *receiveScreen) keyPress(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.askQuit {
		switch yesNo(msg) {
		case 1:
			return s, s.op.stopThenQuit()
		case -1:
			s.askQuit = false
		}
		return s, nil
	}
	switch s.state {
	case recvHost:
		switch {
		case key.Matches(msg, keys.Cancel):
			return s, popScreen()
		case msg.Code == tea.KeyEnter:
			s.state = recvResolving
			return s, resolve(s.svc.ctx, s.input.Value())
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd

	case recvCode:
		switch {
		case key.Matches(msg, keys.Cancel):
			s.op.stop()
			return s, nil
		case msg.Code == tea.KeyEnter:
			c, err := transfer.ParseCode(s.code.Value())
			s.code.SetValue("")
			if err != nil {
				s.note = err.Error()
				return s, nil
			}
			s.note = ""
			s.state = recvAuth
			select {
			case s.codeCh <- c:
			default:
			}
			return s, nil
		}
		var cmd tea.Cmd
		s.code, cmd = s.code.Update(msg)
		return s, cmd

	case recvPlace:
		opts := s.options()
		p := s.projects[s.current]
		switch {
		case key.Matches(msg, keys.Cancel):
			if s.file != "" {
				return s, tea.Batch(popScreen(), status("import cancelled; nothing written"))
			}
			s.op.stop()
		case key.Matches(msg, keys.Quit):
			s.askQuit = true
		case key.Matches(msg, keys.Up):
			s.cursor = max(0, s.cursor-1)
		case key.Matches(msg, keys.Down):
			s.cursor = min(len(opts)-1, s.cursor+1)
		case msg.Code == tea.KeyEnter:
			switch {
			case s.cursor < len(p.candidates):
				dir := p.candidates[s.cursor].Dir
				if n, err := store.NormalizePath(dir); err == nil {
					dir = n
				}
				s.place(dir)
			case s.cursor == len(p.candidates):
				s.state = recvPath
				s.note = ""
				s.input.SetValue("")
			default:
				s.place("") // as-is
			}
		}
		return s, nil

	case recvPath:
		switch {
		case key.Matches(msg, keys.Cancel):
			s.state = recvPlace
			return s, nil
		case msg.Code == tea.KeyEnter:
			raw := strings.TrimSpace(s.input.Value())
			dir, err := store.NormalizePath(raw)
			if raw == "" || err != nil || !isDir(dir) {
				s.note = fmt.Sprintf("%q is not a directory here", raw)
				return s, nil
			}
			s.note = ""
			s.place(dir)
			return s, nil
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd

	case recvConfirm:
		if s.box.key(msg) {
			return s, nil
		}
		switch yesNo(msg) {
		case 1:
			if s.file != "" {
				return s.startFile()
			}
			s.state = recvImporting
			select {
			case s.answerCh <- recvAnswer{rules: s.rules(), ok: true}:
			default:
			}
		case -1:
			if s.file != "" {
				return s, tea.Batch(popScreen(), status("import cancelled; nothing written"))
			}
			select {
			case s.answerCh <- recvAnswer{}:
			default:
			}
		default:
			switch {
			case key.Matches(msg, keys.Cancel): // ctrl+c
				s.op.stop()
			case key.Matches(msg, keys.Quit):
				s.askQuit = true
			}
		}
		return s, nil

	default: // resolving, connecting, auth, importing
		switch {
		case key.Matches(msg, keys.Cancel):
			if s.op == nil {
				// Still resolving the host: nothing runs yet, so esc
				// simply leaves (the late answer lands on no screen).
				return s, popScreen()
			}
			s.op.stop()
		case key.Matches(msg, keys.Quit):
			s.askQuit = true
		}
		return s, nil
	}
}

// asIs marks the current project as-is: undecided projects that were
// skipped stay unmapped, which porter lands under the original slug.
func (p *recvProject) asIs() bool { return !p.exists && p.mapped == "" }

// summaryLines renders the manifest summary with the placements so far.
func (s *receiveScreen) summaryLines() []string {
	lines := []string{"Bundle " + s.header + ":"}
	for i, p := range s.projects {
		placement := "exists here ✗ → imported as-is; rehome later with r"
		switch {
		case p.exists:
			placement = "exists here ✓ (same directory — no rehome needed)"
		case p.mapped != "":
			placement = "exists here ✗ → " + shortPath(p.mapped) + " (relocated record appended)"
		case s.state == recvPlace && i == s.current:
			placement = "exists here ✗"
		}
		lines = append(lines, fmt.Sprintf("  project %s        %s", p.label, placement))
		line := fmt.Sprintf("    %s  %s", countNoun(p.sessions, "session"), formatSize(p.bytes))
		if p.memoryFiles > 0 {
			line += fmt.Sprintf("   memory %d files", p.memoryFiles)
		}
		if p.trust != "" {
			line += fmt.Sprintf("   (trust: %s on the other machine — informational)", p.trust)
		}
		lines = append(lines, line)
		if p.mapped != "" {
			if slug, err := transcripts.Slug(p.mapped); err == nil {
				lines = append(lines, fmt.Sprintf("    → sessions will be placed under projects/%s/ (relocated record appended)", slug))
			}
			if p.memoryFiles > 0 {
				if dir, err := transcripts.MemoryDirFor(s.root, p.mapped); err == nil {
					lines = append(lines, "    → memory merged into "+shortPath(dir))
				}
			}
		}
		if p.memoryFiles > 0 {
			target := p.label
			switch {
			case p.mapped != "":
				target = shortPath(p.mapped)
			case p.asIs():
				target = "projects/" + transcripts.Sanitize(p.slug) + "/memory (as-is)"
			}
			lines = append(lines, "    note: memory files in this bundle will be loaded into every future claude session for "+target,
				"          (pinned files arrive unpinned)")
		}
	}
	acct := s.account
	if acct == "" {
		acct = transcripts.HomeName
	}
	retention := "never swept (cleanupPeriodDays 0)"
	if s.days > 0 {
		retention = fmt.Sprintf("%d days (%s)", s.days, s.daysSrc)
	}
	lines = append(lines, fmt.Sprintf("Target: account %q → %s   limit %s   retention: %s", acct, shortPath(s.root.Dir), formatSize(s.limit), retention))
	return lines
}

// mouse scrolls the confirmation summary; every other state is
// read-only or driven by a prompt.
func (s *receiveScreen) mouse(msg tea.MouseMsg, _, _ int) tea.Cmd {
	if s.state == recvConfirm {
		s.box.mouse(msg)
	}
	return nil
}

func (s *receiveScreen) View(width, height int) string {
	head := styleFaint.Render(truncate("receive into "+shortRootLabel(s.root)+"  ("+shortPath(s.root.Dir)+")", width))
	if s.file != "" {
		head = styleFaint.Render(truncate("import "+shortPath(s.file)+" into "+shortRootLabel(s.root)+"  ("+shortPath(s.root.Dir)+")", width))
	}
	var lines []string
	switch s.state {
	case recvPeeking:
		return strings.Join([]string{head, "", "reading the bundle's manifest…"}, "\n")
	case recvHost, recvResolving:
		lines = []string{head, "", s.input.View()}
		switch {
		case s.state == recvResolving:
			lines = append(lines, "checking that the address is on this machine's local network…")
		case s.note != "":
			lines = append(lines, styleError.Render(truncate(s.note, width)))
		default:
			lines = append(lines, styleFaint.Render(truncate("only an address on a local network of this machine is dialled; the other machine runs bffs export --serve (or s here)", width)))
		}
		return strings.Join(lines, "\n")
	case recvConnecting:
		lines = []string{head, "", fmt.Sprintf("connecting to %s…", fromTarget(s.addr.Addr(), s.addr.Port()))}
	case recvCode:
		lines = append([]string{head, ""}, s.events...)
		lines = append(lines, "", s.code.View())
		if s.note != "" {
			lines = append(lines, styleError.Render(truncate(s.note, width)))
		} else {
			lines = append(lines, styleFaint.Render("XXXX-XXXX as shown there; case, dashes and spaces do not matter; enter sends it, esc cancels"))
		}
		return strings.Join(lines, "\n")
	case recvAuth:
		lines = append([]string{head, ""}, s.events...)
		lines = append(lines, "", "waiting for the manifest…")
	case recvPlace, recvPath:
		lines = append([]string{head, ""}, s.summaryLines()...)
		p := s.projects[s.current]
		lines = append(lines, "", truncate(fmt.Sprintf("where does %s live on this machine?", p.label), width))
		for i, o := range s.options() {
			line := pad(fmt.Sprintf("[%d] %s", i+1, o), max(0, width-2))
			if i == s.cursor && s.state == recvPlace {
				line = styleCursor.Render("> " + line)
			} else {
				line = "  " + line
			}
			lines = append(lines, truncate(line, width))
		}
		if s.state == recvPath {
			lines = append(lines, "", s.input.View())
			if s.note != "" {
				lines = append(lines, styleError.Render(truncate(s.note, width)))
			}
		}
		return strings.Join(lines, "\n")
	case recvConfirm:
		return s.box.view([]string{head, ""}, s.summaryLines(), []string{"", fmt.Sprintf("Import into %s? [y/N]", shortPath(s.root.ConfigDir))}, width, height)
	case recvImporting:
		lines = append([]string{head, ""}, s.events...)
		lines = append(lines, "", s.prog.view(width))
		if s.file != "" {
			lines = append(lines, styleFaint.Render("every file is verified against the manifest before it lands; a session is whole or absent"))
		}
	}
	switch {
	case s.askQuit:
		lines = append(lines, "", quitPrompt)
	case s.op.cancelled():
		lines = append(lines, "", cancelling)
	default:
		lines = append(lines, "", styleFaint.Render("esc cancels"))
	}
	return strings.Join(lines, "\n")
}
