package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

// op is one long operation (plan §11 "Long ops"): a goroutine that owns
// the work, a context the screen cancels with esc or ctrl+c, and one
// channel the goroutine feeds — bundle.Progress, transfer events and the
// screen's own final message — which the screen drains with wait(),
// re-arming the command after every message. Progress and events are
// sent without blocking (a slow reader loses an intermediate frame,
// never the outcome); the final message is always delivered (the buffer
// holds it, or the send waits for room) and the channel closes behind it.
type op struct {
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan tea.Msg
	done   bool
}

// opChannelCap is how many undelivered messages an op holds before it
// drops progress frames; a serve emits a couple of dozen events.
const opChannelCap = 256

// startOp runs fn in a goroutine and returns the op together with the
// first wait command. fn reports through emit (progress, events) and
// returns the final message; a nil final message ends the op silently.
func startOp(parent context.Context, fn func(ctx context.Context, emit func(tea.Msg)) tea.Msg) (*op, tea.Cmd) {
	ctx, cancel := context.WithCancel(parent)
	o := &op{ctx: ctx, cancel: cancel, ch: make(chan tea.Msg, opChannelCap)}
	go func() {
		final := fn(ctx, o.emit)
		if final != nil {
			o.ch <- final
		}
		close(o.ch)
		cancel()
	}()
	return o, o.wait()
}

// emit is the non-blocking send the goroutine uses for intermediate
// messages.
func (o *op) emit(m tea.Msg) {
	select {
	case o.ch <- m:
	default:
	}
}

// progress adapts emit to the bundle.Progress callback shape.
func (o *op) progress(p bundle.Progress) { o.emit(progressMsg{p: p}) }

// event adapts emit to the transfer.Event callback shape.
func (o *op) event(ev transfer.Event) { o.emit(transferEventMsg{ev: ev}) }

// wait is the command that delivers the next message of the op; a closed
// channel yields nil, which bubbletea ignores.
func (o *op) wait() tea.Cmd {
	if o == nil {
		return nil
	}
	return func() tea.Msg {
		m, ok := <-o.ch
		if !ok {
			return nil
		}
		return m
	}
}

// active reports whether the goroutine may still send: the op exists and
// its final message has not been seen.
func (o *op) active() bool { return o != nil && !o.done }

// finish marks the final message as seen.
func (o *op) finish() { o.done = true }

// stop cancels the operation (idempotent).
func (o *op) stop() {
	if o != nil {
		o.cancel()
	}
}

// cancelled reports whether stop was called (or the parent context
// ended) on a running op.
func (o *op) cancelled() bool { return o != nil && o.ctx.Err() != nil }

// quitGrace bounds how long the confirmed quit waits for a cancelled
// operation to unwind before the program ends anyway.
var quitGrace = 5 * time.Second

// stopThenQuit is the command behind "quit anyway? [y]": it cancels the
// operation, waits (bounded by quitGrace) for the goroutine to report —
// so a session in flight is rolled back before the process ends — and
// then quits.
func (o *op) stopThenQuit() tea.Cmd {
	o.stop()
	if o == nil || o.done {
		return quit()
	}
	ch := o.ch
	return func() tea.Msg {
		timer := time.NewTimer(quitGrace)
		defer timer.Stop()
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return quitMsg{}
				}
			case <-timer.C:
				return quitMsg{}
			}
		}
	}
}

// opView is the live part of an action screen while its op runs: a bar
// over the bytes, a bubbles/spinner beside the current item, and the
// last bundle.Progress. The spinner advances on every message the op
// delivers and on countdown ticks — never on its own timer, so a model
// test that runs every command terminates. The bar is drawn here rather
// than with bubbles/progress: that package pulls in harmonica, which is
// not in go.sum, and the action PR touches nothing outside this package.
type opView struct {
	label string
	spin  spinner.Model
	last  bundle.Progress
	seen  bool
}

func newOpView(label string) opView {
	return opView{
		label: label,
		spin:  spinner.New(spinner.WithSpinner(spinner.Line)),
	}
}

// barWidth is the bar's cell count.
const barWidth = 20

// barBlocks renders a fraction as filled and empty blocks, the way the
// CLI's progress bar does.
func barBlocks(pct float64) string {
	n := int(pct*barWidth + 0.5)
	n = max(0, min(barWidth, n))
	return strings.Repeat("█", n) + strings.Repeat("░", barWidth-n)
}

// update records a progress frame and turns the spinner.
func (v *opView) update(p bundle.Progress) {
	v.last, v.seen = p, true
	v.turn()
}

// turn advances the spinner one frame.
func (v *opView) turn() {
	v.spin, _ = v.spin.Update(v.spin.Tick())
}

// view renders "label  bar  pct  bytes  spinner current" cut to width.
func (v opView) view(width int) string {
	p := v.last
	pct := 0.0
	if p.TotalBytes > 0 {
		pct = min(1, float64(p.Bytes)/float64(p.TotalBytes))
	}
	var sb strings.Builder
	sb.WriteString(v.label)
	sb.WriteString("  ")
	sb.WriteString(barBlocks(pct))
	if v.seen {
		sb.WriteString(fmt.Sprintf(" %3.0f%%  %s / %s", pct*100, formatSize(p.Bytes), formatSize(p.TotalBytes)))
		if p.TotalFiles > 0 {
			sb.WriteString(fmt.Sprintf("  %d/%d files", p.Files, p.TotalFiles))
		}
	}
	sb.WriteString("  ")
	sb.WriteString(v.spin.View())
	if v.seen && p.Current != "" {
		sb.WriteString(" ")
		sb.WriteString(transcripts.Sanitize(p.Current))
	}
	return truncate(sb.String(), width)
}

// newInput is a single-line text field with the browser's conventions:
// a static cursor (a blinking one schedules ticks a model test would
// have to run) and focus from the start.
func newInput(prompt, placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.Placeholder = placeholder
	st := ti.Styles()
	st.Cursor.Blink = false
	ti.SetStyles(st)
	_ = ti.Focus()
	return ti
}

// resultScreen shows what an action did — the CLI's receipt lines — in
// a viewport; esc or enter pops it and reloads the list underneath.
type resultScreen struct {
	title string
	lines []string
	vp    viewport.Model
	err   error
}

func newResultScreen(title string, lines []string, err error) *resultScreen {
	s := &resultScreen{title: title, lines: lines, vp: newViewport(), err: err}
	content := make([]string, 0, len(lines)+2)
	if err != nil {
		content = append(content, styleError.Render(truncate(transcripts.Sanitize(err.Error()), 200)), "")
	}
	for _, l := range lines {
		content = append(content, transcripts.Sanitize(l))
	}
	s.vp.SetContentLines(content)
	return s
}

func (s *resultScreen) Init() tea.Cmd       { return nil }
func (s *resultScreen) Title() string       { return s.title }
func (s *resultScreen) Keys() []key.Binding { return append(viewportKeys(), keys.Done) }

func (s *resultScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.vp.SetWidth(msg.Width)
		s.vp.SetHeight(msg.Height)
		return s, nil
	case tea.KeyPressMsg:
		if key.Matches(msg, keys.Done) {
			return s, popRefresh()
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *resultScreen) View(width, height int) string { return s.vp.View() }

// tickEvery is the countdown period; a test may shorten it.
var tickEvery = time.Second

// tick schedules one tickMsg for id after tickEvery.
func tick(id int) tea.Cmd {
	return tea.Tick(tickEvery, func(time.Time) tea.Msg { return tickMsg{id: id} })
}

// fmtMMSS renders a duration as M:SS ("10:00"); negative is 0:00.
func fmtMMSS(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// yesNo is the [y/N] answer of a key press: 1 yes, -1 no, 0 neither.
func yesNo(msg tea.KeyPressMsg) int {
	switch {
	case key.Matches(msg, keys.Yes):
		return 1
	case key.Matches(msg, keys.No):
		return -1
	}
	return 0
}

// quitPrompt is the question an action screen asks when q is pressed
// while its operation runs.
const quitPrompt = "an operation is running — quit anyway? it is cancelled first [y/N]"

// cancelling is the line shown between esc and the operation's end.
const cancelling = "cancelling…"

// joinLines renders lines for a screen, each sanitised and cut to width.
// scrollBox keeps a confirmation readable when its summary is longer
// than the pane: the head and the footer (the [y/N] prompt, which must
// never be off-screen) stay put and the body scrolls between them.
type scrollBox struct{ offset int }

// view lays head, a window of body and footer into height lines.
func (b *scrollBox) view(head, body, footer []string, width, height int) string {
	out := append([]string{}, head...)
	avail := max(1, height-len(head)-len(footer))
	if len(body) <= avail {
		b.offset = 0
		out = append(out, body...)
	} else {
		inner := max(1, avail-1)
		if b.offset > len(body)-inner {
			b.offset = len(body) - inner
		}
		if b.offset < 0 {
			b.offset = 0
		}
		end := min(len(body), b.offset+inner)
		out = append(out, body[b.offset:end]...)
		out = append(out, fmt.Sprintf("  ↑/↓ scroll · lines %d-%d of %d", b.offset+1, end, len(body)))
	}
	return joinLines(append(out, footer...), width)
}

// key handles the scrolling keys; ok is false for anything else, which
// the screen then answers itself.
func (b *scrollBox) key(msg tea.KeyPressMsg) (ok bool) {
	switch {
	case key.Matches(msg, keys.Up):
		b.offset--
	case key.Matches(msg, keys.Down):
		b.offset++
	case key.Matches(msg, keys.PageUp):
		b.offset -= 10
	case key.Matches(msg, keys.PageDn):
		b.offset += 10
	default:
		return false
	}
	if b.offset < 0 {
		b.offset = 0
	}
	return true
}

// scrollKeys are the bindings a scrollable confirmation shows.
func scrollKeys() []key.Binding { return []key.Binding{keys.Up, keys.Down} }

func joinLines(lines []string, width int) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = truncate(transcripts.Sanitize(l), width)
	}
	return strings.Join(out, "\n")
}
