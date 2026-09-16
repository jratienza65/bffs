package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// exportState is where the ExportFile screen is.
type exportState int

const (
	exportInput     exportState = iota // typing the output path
	exportPreparing                    // porter.Select + BuildManifest (the hashing pre-pass)
	exportConfirm                      // summary + [y/N]
	exportWriting                      // porter.Write into a temp file beside the path
)

// exportPreparedMsg ends the pre-pass.
type exportPreparedMsg struct {
	m        *bundle.Manifest
	opener   bundle.Opener
	warnings []string
	err      error
}

// exportDoneMsg ends the write.
type exportDoneMsg struct {
	size int64
	err  error
}

// exportScreen is `bffs export --out <path>` for the target: a path
// input (an existing file is refused), the CLI's summary block, [y/N],
// a progress bar while the bundle is written, and a result naming the
// path and the bundle id.
type exportScreen struct {
	svc      *services
	tgt      actionTarget
	state    exportState
	input    textinput.Model
	path     string
	note     string // validation message under the input
	op       *op
	prog     opView
	m        *bundle.Manifest
	opener   bundle.Opener
	summary  []string
	warnings []string
	askQuit  bool
	width    int
}

func newExportScreen(svc *services, tgt actionTarget) *exportScreen {
	cwd := svc.cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	in := newInput("to: ", "path of the .bffs file")
	in.SetValue(filepath.Join(cwd, defaultBundleName(svc.now())))
	in.CursorEnd()
	return &exportScreen{svc: svc, tgt: tgt, input: in, prog: newOpView("hashing")}
}

func (s *exportScreen) Init() tea.Cmd        { return nil }
func (s *exportScreen) Title() string        { return "export" }
func (s *exportScreen) capturingInput() bool { return s.state == exportInput }
func (s *exportScreen) running() bool        { return s.op.active() }

func (s *exportScreen) Keys() []key.Binding {
	switch s.state {
	case exportInput:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "export here")), keys.Cancel}
	case exportConfirm:
		return []key.Binding{keys.Yes, keys.No}
	}
	return []key.Binding{keys.Cancel}
}

// release closes the opener when the bundle will not be written.
func (s *exportScreen) release() {
	if c, ok := s.opener.(io.Closer); ok {
		_ = c.Close()
	}
	s.opener = nil
}

// exportAccount is the account an export runs as — source.account in
// the manifest: the root's owner, else what the resolver picks for the
// cwd. Isolation is the oauth preset in force.
func exportAccount(svc *services, root transcripts.Root) (store.Account, store.IsolationPreset) {
	name := destAccount(svc, root)
	acc, ok := svc.accs.Get(name)
	if !ok {
		return store.Account{}, ""
	}
	if acc.Type != store.TypeOAuth {
		return acc, ""
	}
	return acc, store.ResolveIsolation(acc.Isolation, svc.state.Isolation)
}

// exportOptions is the ExportOptions every export and serve of the
// browser builds with; progress goes to the op.
func exportOptions(svc *services, root transcripts.Root, progress func(bundle.Progress)) porter.ExportOptions {
	acc, iso := exportAccount(svc, root)
	return porter.ExportOptions{
		Compression: bundle.CompGzip,
		Version:     svc.version,
		Account:     acc,
		Isolation:   iso,
		Now:         svc.now(),
		Progress:    progress,
	}
}

// prepareExport is the op that resolves the selection and builds the
// manifest (hashing every file, Phase "hash").
func prepareExport(svc *services, tgt actionTarget) func(context.Context, func(tea.Msg)) tea.Msg {
	return func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
		sel, _, warnings, err := tgt.selection(ctx, svc)
		if err != nil {
			return exportPreparedMsg{err: err}
		}
		opts := exportOptions(svc, tgt.root, func(p bundle.Progress) { emit(progressMsg{p: p}) })
		m, opener, more, err := porter.BuildManifest(ctx, sel, opts)
		if err != nil {
			return exportPreparedMsg{err: err}
		}
		return exportPreparedMsg{m: m, opener: opener, warnings: append(warnings, more...)}
	}
}

// validateOut resolves what was typed: non-empty, normalised like every
// user path, and not an existing file.
func validateOut(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("type a path")
	}
	path, err := store.NormalizePath(raw)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", shortPath(path))
		}
		return "", fmt.Errorf("%s exists; choose another name", shortPath(path))
	}
	if !isDir(filepath.Dir(path)) {
		return "", fmt.Errorf("directory %s does not exist", shortPath(filepath.Dir(path)))
	}
	return path, nil
}

// writeBundleFile streams the bundle into a temporary file beside path
// (mode 0600) and renames it into place once complete and synced, so an
// interrupted export never leaves a truncated .bffs under the final
// name. The opener is closed by porter.Write either way.
func writeBundleFile(ctx context.Context, path string, m *bundle.Manifest, src bundle.Opener, opts porter.ExportOptions) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		if c, ok := src.(io.Closer); ok {
			_ = c.Close()
		}
		return 0, err
	}
	tmpPath := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(tmp, 256<<10)
	cw := &countingWriter{w: bw}
	if _, err := porter.Write(ctx, cw, m, src, opts); err != nil {
		return 0, err
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	// The path was free when it was typed; the hashing pre-pass and the
	// confirmation may have taken minutes, and a rename would replace
	// whatever appeared meanwhile — an existing file is never overwritten.
	if _, err := os.Lstat(path); err == nil {
		return 0, fmt.Errorf("%s appeared while the bundle was being written; choose another name", shortPath(path))
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		done = true
		return 0, err
	}
	done = true
	return cw.n, nil
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (s *exportScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.input.SetWidth(max(10, msg.Width-6))
		return s, nil

	case progressMsg:
		s.prog.update(msg.p)
		return s, s.op.wait()

	case exportPreparedMsg:
		s.op.finish()
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) || s.op.cancelled() {
				return s, tea.Batch(popScreen(), status("export cancelled; nothing written"))
			}
			return s, replaceScreen(newResultScreen("export", nil, msg.err))
		}
		s.m, s.opener, s.warnings = msg.m, msg.opener, msg.warnings
		s.summary = exportSummaryLines(s.tgt.root, msg.m, msg.opener, s.tgt.partsOrDefault(), s.svc.now())
		s.state = exportConfirm
		return s, nil

	case exportDoneMsg:
		s.op.finish()
		s.opener = nil // porter.Write closed it
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) || s.op.cancelled() {
				return s, tea.Batch(popScreen(), status("export cancelled; nothing written"))
			}
			return s, replaceScreen(newResultScreen("export", nil, fmt.Errorf("export: %w", msg.err)))
		}
		nSessions, nMemoryFiles := manifestCounts(s.m)
		lines := []string{
			fmt.Sprintf("wrote %s (%s, %s, %d memory files, bundle %s)", shortPath(s.path), formatSize(msg.size), countNoun(nSessions, "session"), nMemoryFiles, short8(s.m.BundleID)),
			"",
			"On the other machine: bffs import --from " + filepath.Base(s.path),
		}
		for _, w := range s.warnings {
			lines = append(lines, "warning: "+w)
		}
		return s, replaceScreen(newResultScreen("export", lines, nil))

	case tea.KeyPressMsg:
		return s.key(msg)
	}
	if s.state == exportInput {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	}
	return s, nil
}

func (s *exportScreen) key(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
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
	case exportInput:
		switch {
		case key.Matches(msg, keys.Cancel):
			return s, popScreen()
		case msg.Code == tea.KeyEnter:
			path, err := validateOut(s.input.Value())
			if err != nil {
				s.note = err.Error()
				return s, nil
			}
			s.path, s.note = path, ""
			s.state = exportPreparing
			var cmd tea.Cmd
			s.op, cmd = startOp(s.svc.ctx, prepareExport(s.svc, s.tgt))
			return s, cmd
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd

	case exportConfirm:
		switch yesNo(msg) {
		case 1:
			s.state = exportWriting
			s.prog = newOpView("writing")
			m, opener, path, svc, root := s.m, s.opener, s.path, s.svc, s.tgt.root
			var cmd tea.Cmd
			s.op, cmd = startOp(s.svc.ctx, func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
				opts := exportOptions(svc, root, func(p bundle.Progress) { emit(progressMsg{p: p}) })
				size, err := writeBundleFile(ctx, path, m, opener, opts)
				return exportDoneMsg{size: size, err: err}
			})
			return s, cmd
		case -1:
			s.release()
			return s, tea.Batch(popScreen(), status("export aborted; nothing written"))
		}
		return s, nil

	default: // an op runs
		switch {
		case key.Matches(msg, keys.Cancel):
			s.op.stop()
		case key.Matches(msg, keys.Quit):
			s.askQuit = true
		}
		return s, nil
	}
}

func (s *exportScreen) View(width, height int) string {
	head := styleFaint.Render(truncate("export "+s.tgt.what()+"  from "+shortRootLabel(s.tgt.root), width))
	switch s.state {
	case exportInput:
		lines := []string{head, "", s.input.View()}
		if s.note != "" {
			lines = append(lines, styleError.Render(truncate(s.note, width)))
		} else {
			lines = append(lines, styleFaint.Render(truncate("enter exports here · esc goes back · an existing file is never overwritten", width)))
		}
		return strings.Join(lines, "\n")
	case exportConfirm:
		lines := append([]string{head, ""}, s.summary...)
		for _, w := range s.warnings {
			lines = append(lines, "warning: "+w)
		}
		lines = append(lines, "", fmt.Sprintf("Write %s? [y/N]", shortPath(s.path)))
		return joinLines(lines, width)
	}
	lines := []string{head, ""}
	if s.state == exportWriting {
		lines = append(lines, joinLines(s.summary, width), "")
	}
	lines = append(lines, s.prog.view(width))
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
