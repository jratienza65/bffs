package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/user"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

// The plan's serve defaults (§8): port 7345, a ten-minute pairing
// window, three wrong codes end the serve.
const (
	servePort     = 7345
	serveTTL      = 10 * time.Minute
	serveAttempts = 3
)

// Injection seams for the loopback tests, the same ones cmd has: how a
// serve binds, which local addresses both sides see, how a fetch dials
// and resolves, and which code a serve shows. Production uses the real
// ones (a nil Listen or Dial is net.Listen / net.Dialer inside transfer).
var (
	serveListen   transfer.Listener
	serveLocal    = transfer.LANAddrs
	serveGenerate = transfer.GenerateCode
	serveBindPort = servePort
	fetchLocal    = transfer.LANAddrs
	fetchDial     func(ctx context.Context, network, addr string) (net.Conn, error)
	// lanOptions is what counts as the local network for both sides; the
	// browser offers no --allow-routed, so only the tests widen it.
	lanOptions = transfer.LANOptions{}
)

// serveState is where the Serve screen is.
type serveState int

const (
	servePreparing serveState = iota // porter.Select + BuildManifest
	serveConfirm                     // summary + [y/N]
	serveServing                     // transfer.Serve runs
)

// serveDoneMsg ends the serve.
type serveDoneMsg struct {
	res transfer.ServeResult
	err error
}

// serveScreen is `bffs export --serve` for the target: the summary,
// [y/N], then a static screen with the banner — the import command for
// the other machine, the pairing code rendered large, this machine's
// key fingerprint — a countdown, the event log and the sending bar. The
// code is rendered on this screen only: never in the status line, the
// log, an error or a result.
type serveScreen struct {
	svc      *services
	tgt      actionTarget
	state    serveState
	lan      transfer.LANOptions
	local    []transfer.LinkAddr
	op       *op
	prog     opView
	m        *bundle.Manifest
	raw      []byte // the manifest bytes transfer sends ahead of the body
	opener   bundle.Opener
	summary  []string
	warnings []string

	code     transfer.Code
	deadline time.Time
	tickID   int
	addr     netip.AddrPort // the first bound address, from the listen event
	keyFP    string         // this machine's key fingerprint
	bound    bool
	events   []string
	sending  bool
	askQuit  bool
	width    int
	box      scrollBox
}

// newServeScreen checks the local network first (the CLI's prepareServe):
// a machine with only loopback/VPN interfaces up cannot serve, and says
// so before any selection work.
func newServeScreen(svc *services, tgt actionTarget) (*serveScreen, error) {
	lan := lanOptions
	local, err := serveLocal(lan)
	if err != nil {
		return nil, err
	}
	if len(local) == 0 {
		return nil, errors.New("no local-network address found (only loopback/VPN interfaces are up); connect to Wi-Fi/Ethernet or export to a file (e)")
	}
	return &serveScreen{svc: svc, tgt: tgt, lan: lan, local: local, prog: newOpView("hashing")}, nil
}

func (s *serveScreen) Init() tea.Cmd {
	var cmd tea.Cmd
	s.op, cmd = startOp(s.svc.ctx, prepareExport(s.svc, s.tgt))
	return cmd
}

func (s *serveScreen) Title() string { return "send over LAN" }
func (s *serveScreen) running() bool { return s.op.active() }

func (s *serveScreen) Keys() []key.Binding {
	if s.state == serveConfirm {
		return append([]key.Binding{keys.Yes, keys.No}, scrollKeys()...)
	}
	return []key.Binding{keys.Cancel}
}

// userIdent is the local user name the way a manifest constrains it.
func userIdent() string {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:] // DOMAIN\user on Windows
	}
	var sb strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			sb.WriteRune(r)
		}
		if sb.Len() >= 64 {
			break
		}
	}
	return sb.String()
}

// start begins the serve: the code is drawn, the goroutine runs
// transfer.Serve with the manifest bytes and a Body that streams the
// bundle through bundle.Build (porter.Write would close the opener after
// one build, and a serve may stream again after a peer vanished).
func (s *serveScreen) start() (Screen, tea.Cmd) {
	code, err := serveGenerate()
	if err != nil {
		s.release()
		return s, replaceScreen(newResultScreen("send over LAN", nil, fmt.Errorf("pairing code: %w", err)))
	}
	s.code = code
	s.state = serveServing
	s.prog = newOpView("sending")
	s.deadline = time.Now().Add(serveTTL)
	s.tickID++
	account := s.tgt.root.Owner
	if account == "" {
		account = destAccount(s.svc, s.tgt.root)
	}
	m, raw, opener, svc, root := s.m, s.raw, s.opener, s.svc, s.tgt.root
	lan, local := s.lan, s.local
	var cmd tea.Cmd
	s.op, cmd = startOp(s.svc.ctx, func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
		opts := exportOptions(svc, root, func(p bundle.Progress) { emit(progressMsg{p: p}) })
		res, err := transfer.Serve(ctx, transfer.ServeOptions{
			Code:        code,
			Port:        serveBindPort,
			TTL:         serveTTL,
			MaxAttempts: serveAttempts,
			Manifest:    raw,
			Compression: byte(opts.Compression),
			Body: func(ctx context.Context, w io.Writer) error {
				bw := bufio.NewWriterSize(w, 256<<10)
				got, err := bundle.Build(ctx, bw, m, opener, opts.Compression, opts.Progress)
				if err != nil {
					return err
				}
				if !bytes.Equal(got, raw) {
					return errors.New("manifest changed between the summary and the stream")
				}
				return bw.Flush()
			},
			Events:  func(ev transfer.Event) { emit(transferEventMsg{ev: ev}) },
			LAN:     lan,
			Listen:  serveListen,
			Version: svc.version,
			Host:    hostIdent(),
			User:    userIdent(),
			Account: account,
			Local:   local,
		})
		return serveDoneMsg{res: res, err: err}
	})
	return s, tea.Batch(cmd, tick(s.tickID))
}

// release closes the opener when the bundle will not be streamed.
func (s *serveScreen) release() {
	if c, ok := s.opener.(io.Closer); ok {
		_ = c.Close()
	}
	s.opener = nil
}

// eventLine is the log line for one A-side event, the CLI's wording
// where this side has the facts. Peer text is sanitised; code-ok is
// folded into the manifest line, the bar shows the sending, the result
// screen carries the delivery and the expiry.
func serveEventLine(ev transfer.Event, manifestSize int64) string {
	text := ""
	switch ev.Kind {
	case "connect":
		text = peerIP(ev.Peer) + " connected — waiting for its code"
	case "manifest-sent":
		text = fmt.Sprintf("%s code accepted; manifest sent (%s) — waiting for the other side to review", peerIP(ev.Peer), formatSize(manifestSize))
	case "accept":
		if name, ok := strings.CutSuffix(ev.Text, " accepted"); ok {
			text = "manifest accepted by " + transcripts.Sanitize(name)
		} else {
			text = transcripts.Sanitize(ev.Text)
		}
	case "code-ok", "sending", "progress", "done", "expired", "listen":
		return ""
	default:
		text = transcripts.Sanitize(ev.Text)
	}
	return fmt.Sprintf("  %s  %s", ev.Time.Format("15:04:05"), text)
}

// peerIP is the address part of an event's ip:port peer, sanitised.
func peerIP(peer string) string {
	if ap, err := netip.ParseAddrPort(peer); err == nil {
		return ap.Addr().String()
	}
	return transcripts.Sanitize(peer)
}

// keyFromText picks the key fingerprint out of a transfer event text
// ("… key 3f9a1c2e" / "… peer key 3f9a1c2e)"): the hex run after marker.
func keyFromText(text, marker string) string {
	i := strings.LastIndex(text, marker)
	if i < 0 {
		return "unknown"
	}
	t := text[i+len(marker):]
	n := 0
	for n < len(t) && n < 8 && isHexByte(t[n]) {
		n++
	}
	if n == 0 {
		return "unknown"
	}
	return t[:n]
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// fromTarget renders an address the way --from takes it: IPv6 in
// brackets, the port only when it is not the default.
func fromTarget(addr netip.Addr, port uint16) string {
	str := addr.WithZone("").String()
	if addr.Is6() {
		str = "[" + str + "]"
	}
	if port != servePort {
		str += fmt.Sprintf(":%d", port)
	}
	return str
}

func (s *serveScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		return s, nil

	case progressMsg:
		s.sending = true
		s.prog.update(msg.p)
		return s, s.op.wait()

	case transferEventMsg:
		ev := msg.ev
		if ev.Kind == "listen" && !s.bound {
			s.bound = true
			s.keyFP = keyFromText(ev.Text, "key ")
			if ap, err := netip.ParseAddrPort(ev.Peer); err == nil {
				s.addr = ap
			}
		}
		if line := serveEventLine(ev, int64(len(s.raw))); line != "" {
			s.events = append(s.events, line)
		}
		s.prog.turn()
		return s, s.op.wait()

	case tickMsg:
		if msg.id != s.tickID || !s.op.active() || s.state != serveServing {
			return s, nil
		}
		s.prog.turn()
		return s, tick(s.tickID)

	case exportPreparedMsg:
		s.op.finish()
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) || s.op.cancelled() {
				return s, tea.Batch(popScreen(), status("cancelled; nothing was sent"))
			}
			return s, replaceScreen(newResultScreen("send over LAN", nil, msg.err))
		}
		// The manifest bytes transfer sends ahead of the body must be
		// entry 0 of that body byte for byte: bundle.Build marshals with
		// the same compact json.Marshal (Body checks it).
		raw, err := json.Marshal(msg.m)
		if err != nil {
			s.opener = msg.opener
			s.release()
			return s, replaceScreen(newResultScreen("send over LAN", nil, fmt.Errorf("marshal manifest: %w", err)))
		}
		s.m, s.raw, s.opener, s.warnings = msg.m, raw, msg.opener, msg.warnings
		s.summary = exportSummaryLines(s.tgt.root, msg.m, msg.opener, s.tgt.partsOrDefault(), s.svc.now())
		s.state = serveConfirm
		return s, nil

	case serveDoneMsg:
		s.op.finish()
		s.release()
		return s.finish(msg)

	case tea.KeyPressMsg:
		return s.key(msg)
	}
	return s, nil
}

// finish maps the serve's outcome to a result screen or a status line,
// the exit paths of the CLI's serveExport.
func (s *serveScreen) finish(msg serveDoneMsg) (Screen, tea.Cmd) {
	res, err := msg.res, msg.err
	switch {
	case err == nil:
		who := transcripts.Sanitize(res.PeerHost)
		if res.PeerUser != "" {
			who += " (" + transcripts.Sanitize(res.PeerUser) + ")"
		}
		lines := []string{
			fmt.Sprintf("delivered: %d files (%s) verified by %s in %s", res.Done.Entries, formatSize(res.Bytes), who, res.Duration.Round(100*time.Millisecond)),
			fmt.Sprintf("bundle %s, %d pairing attempt(s)", short8(s.m.BundleID), res.Attempts),
			"Done.",
		}
		return s, replaceScreen(newResultScreen("send over LAN", lines, nil))
	case s.op.cancelled() || errors.Is(err, context.Canceled):
		if res.Bytes > 0 {
			return s, tea.Batch(popScreen(), status(fmt.Sprintf("cancelled after %s; the other machine discards the partial bundle", formatSize(res.Bytes))))
		}
		return s, tea.Batch(popScreen(), status("cancelled; nothing was sent"))
	case errors.Is(err, transfer.ErrTooManyAttempts):
		return s, replaceScreen(newResultScreen("send over LAN", s.events, fmt.Errorf("%d failed pairing attempts; press s again for a new code", serveAttempts)))
	default:
		return s, replaceScreen(newResultScreen("send over LAN", s.events, err))
	}
}

func (s *serveScreen) key(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.askQuit {
		switch yesNo(msg) {
		case 1:
			return s, s.op.stopThenQuit()
		case -1:
			s.askQuit = false
		}
		return s, nil
	}
	if s.state == serveConfirm {
		if s.box.key(msg) {
			return s, nil
		}
		switch yesNo(msg) {
		case 1:
			return s.start()
		case -1:
			s.release()
			return s, tea.Batch(popScreen(), status("aborted; nothing was sent"))
		}
		return s, nil
	}
	switch {
	case key.Matches(msg, keys.Cancel):
		s.op.stop()
	case key.Matches(msg, keys.Quit):
		s.askQuit = true
	}
	return s, nil
}

var styleCode = lipgloss.NewStyle().Bold(true).Padding(0, 3).Border(lipgloss.ThickBorder())

// banner renders the plan's A-side banner: the import command with this
// machine's first bound address, the code in a box beside the key
// fingerprint, and the waiting line with the countdown.
func (s *serveScreen) banner(width int) []string {
	target := "…"
	if s.bound {
		target = fromTarget(s.addr.Addr(), s.addr.Port())
	}
	lines := []string{
		truncate("On the other machine, run:    bffs import --from "+target, width),
		"",
	}
	box := strings.Split(styleCode.Render(s.code.Display()), "\n")
	note := fmt.Sprintf("   pairing code — this machine's key: %s (the other side shows it as \"peer key\")", s.keyFP)
	for i, l := range box {
		if i == len(box)/2 {
			l += note
		}
		lines = append(lines, l)
	}
	left := time.Until(s.deadline)
	lines = append(lines, "",
		truncate(fmt.Sprintf("Waiting for the other machine…  code valid for %s, %d attempts, one transfer.   (esc cancels)", fmtMMSS(left), serveAttempts), width))
	return lines
}

// mouse scrolls the confirmation summary; every other state is
// read-only or driven by a prompt.
func (s *serveScreen) mouse(msg tea.MouseMsg, _, _ int) tea.Cmd {
	if s.state == serveConfirm {
		s.box.mouse(msg)
	}
	return nil
}

func (s *serveScreen) View(width, height int) string {
	head := styleFaint.Render(truncate("send "+s.tgt.what()+"  from "+shortRootLabel(s.tgt.root)+" over the LAN", width))
	switch s.state {
	case servePreparing:
		lines := []string{head, "", s.prog.view(width), ""}
		if s.askQuit {
			lines = append(lines, quitPrompt)
		} else if s.op.cancelled() {
			lines = append(lines, cancelling)
		} else {
			lines = append(lines, styleFaint.Render("esc cancels"))
		}
		return strings.Join(lines, "\n")
	case serveConfirm:
		body := append([]string{}, s.summary...)
		for _, w := range s.warnings {
			body = append(body, "warning: "+w)
		}
		return s.box.view([]string{head, ""}, body,
			[]string{"", "Serve this over the local network? A pairing code is shown next; the other machine runs bffs import --from <this address>. [y/N]"}, width, height)
	}
	lines := append([]string{head, ""}, s.banner(width)...)
	for _, e := range s.events {
		lines = append(lines, truncate(e, width))
	}
	if s.sending {
		lines = append(lines, s.prog.view(width))
	}
	switch {
	case s.askQuit:
		lines = append(lines, "", quitPrompt)
	case s.op.cancelled():
		lines = append(lines, "", cancelling)
	}
	return strings.Join(lines, "\n")
}
