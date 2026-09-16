package skillpack

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/store"
)

// allowedTools is the exact pre-approval set the skill may carry: read and
// plan prefixes only. Every write (rehome apply, trust sync, import, rm,
// switch, every MCP write tool) must go through Claude Code's prompt.
var allowedTools = map[string]bool{
	"Bash(bffs sessions list:*)":     true,
	"Bash(bffs sessions imports:*)":  true,
	"Bash(bffs sessions show:*)":     true,
	"Bash(bffs memory list:*)":       true,
	"Bash(bffs memory scan-paths:*)": true,
	"Bash(bffs rehome --suggest:*)":  true,
	"Bash(bffs trust)":               true,
	"Bash(git remote get-url:*)":     true,
	"Bash(git rev-parse:*)":          true,
	"Bash(ls:*)":                     true,
	"Bash(test:*)":                   true,
	"Read":                           true,
	"Grep":                           true,
	"Glob":                           true,
	"AskUserQuestion":                true,
	"mcp__bffs__list_sessions":       true,
	"mcp__bffs__list_memories":       true,
	"mcp__bffs__trust_status":        true,
}

// embeddedSkill returns the embedded SKILL.md through readAsset, so the
// assertions see the same LF-normalised bytes Install writes even when a
// Windows checkout embedded CRLF.
func embeddedSkill(t *testing.T) string {
	t.Helper()
	raw, err := readAsset(SkillFile)
	if err != nil {
		t.Fatalf("read embedded SKILL.md: %v", err)
	}
	return string(raw)
}

// frontmatterValue returns the value of a single-line frontmatter key.
func frontmatterValue(t *testing.T, doc, key string) string {
	t.Helper()
	end := frontmatterEnd([]byte(doc))
	if end < 0 {
		t.Fatalf("no frontmatter in SKILL.md")
	}
	for _, line := range strings.Split(doc[:end], "\n") {
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("frontmatter key %q missing", key)
	return ""
}

func TestEmbeddedSkillAllowedToolsAreReadOnlyPrefixes(t *testing.T) {
	doc := embeddedSkill(t)
	for _, broad := range []string{"Bash(bffs:*)", "Bash(git:*)", "Bash(find:*)"} {
		if strings.Contains(doc, broad) {
			t.Errorf("SKILL.md pre-approves the broad prefix %q", broad)
		}
	}
	line := frontmatterValue(t, doc, "allowed-tools")
	seen := map[string]bool{}
	for _, entry := range strings.Split(line, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			t.Errorf("allowed-tools has an empty entry: %q", line)
			continue
		}
		if !allowedTools[entry] {
			t.Errorf("allowed-tools entry %q is not in the approved set", entry)
		}
		seen[entry] = true
	}
	for want := range allowedTools {
		if !seen[want] {
			t.Errorf("allowed-tools lacks %q", want)
		}
	}
}

func TestEmbeddedSkillFrontmatterAndMarker(t *testing.T) {
	doc := embeddedSkill(t)
	if got := frontmatterValue(t, doc, "name"); got != SkillName {
		t.Errorf("name: want %q, got %q", SkillName, got)
	}
	if got := frontmatterValue(t, doc, "version"); got != "0.0.0" {
		t.Errorf("embedded version placeholder: want 0.0.0, got %q", got)
	}
	if got := frontmatterValue(t, doc, "argument-hint"); !strings.Contains(got, "bundle-id | session-id... | --all") {
		t.Errorf("argument-hint: %q", got)
	}
	if strings.Contains(doc, "disable-model-invocation") {
		t.Errorf("disable-model-invocation must stay absent so the skill can auto-trigger after an import")
	}
	for _, key := range frontmatterKeys(doc[:frontmatterEnd([]byte(doc))]) {
		switch key {
		case "name", "description", "argument-hint", "allowed-tools", "version":
		default:
			t.Errorf("frontmatter carries the key %q; only name, description, argument-hint, allowed-tools and version are allowed", key)
		}
	}
	end := frontmatterEnd([]byte(doc))
	body := doc[end+len("\n---\n"):]
	firstLine, _, _ := strings.Cut(body, "\n")
	if want := "# bffs-rehome   <!-- managed by bffs; reinstall with `bffs skill install` -->"; firstLine != want {
		t.Errorf("body first line:\n want %q\n got  %q", want, firstLine)
	}
	if !strings.Contains(doc, Marker) {
		t.Errorf("SKILL.md lacks the verbatim marker %q", Marker)
	}
	for _, phrase := range []string{
		"rehome", "the project moved", "resume a session from my other machine",
		"fix imported sessions", "memory still mentions the old path", "folder trust",
	} {
		if !strings.Contains(frontmatterValueBlock(doc, "description"), phrase) {
			t.Errorf("description lacks trigger phrase %q", phrase)
		}
	}
	for _, step := range []string{
		"## 1. Discover", "## 2. Propose", "## 3. Dry run", "## 4. Apply",
		"## 5. Verify", "## 6. Memory", "## 7. Trust", "## Never",
		"bffs sessions imports --json", "bffs sessions list --pending-rehome --json",
		"bffs rehome --suggest --bundle <id>", "--dry-run", "-y",
		"cd <new> && claude --resume <sid>", "bffs memory scan-paths --project <new>",
		"bffs trust sync --to <acct> --project <new>", "--dangerously-skip-permissions",
		"UNTRUSTED", "never delete memories",
	} {
		if !strings.Contains(body, step) {
			t.Errorf("SKILL.md body lacks %q", step)
		}
	}
	checklist, err := readAsset(ChecklistFile)
	if err != nil {
		t.Fatalf("read embedded checklist: %v", err)
	}
	for _, want := range []string{"bffs rehome --suggest", "--map <old>=<new>", "bffs sessions list", "bffs memory scan-paths", "bffs trust sync", Marker} {
		if !bytes.Contains(checklist, []byte(want)) {
			t.Errorf("checklist lacks %q", want)
		}
	}
}

// frontmatterKeys lists the top-level keys of a frontmatter block: every
// line that starts in column one with `key:`; indented continuation lines
// (folded scalars) are not keys.
func frontmatterKeys(front string) []string {
	var keys []string
	for _, line := range strings.Split(front, "\n") {
		if line == "" || line == "---" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		if k, _, ok := strings.Cut(line, ":"); ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// frontmatterValueBlock returns a key's value including folded
// continuation lines (description uses a YAML `>` block).
func frontmatterValueBlock(doc, key string) string {
	end := frontmatterEnd([]byte(doc))
	lines := strings.Split(doc[:end], "\n")
	var b strings.Builder
	in := false
	for _, line := range lines {
		if strings.HasPrefix(line, key+":") {
			in = true
			b.WriteString(strings.TrimPrefix(line, key+":"))
			continue
		}
		if in {
			if strings.HasPrefix(line, " ") {
				b.WriteString(" " + strings.TrimSpace(line))
				continue
			}
			break
		}
	}
	return b.String()
}

func TestRenderSkillStampsVersion(t *testing.T) {
	out, err := renderSkill("1.2.3")
	if err != nil {
		t.Fatalf("renderSkill: %v", err)
	}
	doc := string(out)
	if got := frontmatterValue(t, doc, "version"); got != "1.2.3" {
		t.Errorf("stamped version: want 1.2.3, got %q", got)
	}
	if strings.Contains(doc, "0.0.0") {
		t.Errorf("placeholder survived the stamp")
	}
	// Only the frontmatter changed.
	end := frontmatterEnd(out)
	if !strings.HasSuffix(embeddedSkill(t), doc[end:]) {
		t.Errorf("body changed while stamping")
	}
	raw, err := renderSkill("")
	if err != nil {
		t.Fatalf("renderSkill(\"\"): %v", err)
	}
	if frontmatterValue(t, string(raw), "version") != "0.0.0" {
		t.Errorf("empty version should keep the placeholder")
	}
}

func accounts(accs ...store.Account) store.Accounts {
	a := store.Accounts{Accounts: map[string]store.Account{}}
	for _, acc := range accs {
		a.Accounts[acc.Name] = acc
	}
	return a
}

func TestTargets(t *testing.T) {
	home := t.TempDir()
	cfg := t.TempDir()
	absCfg, _ := filepath.Abs(cfg)

	// partial: its skills/ is a symlink to the home dir's; not a target.
	partialDir := filepath.Join(cfg, "sessions", "personal")
	if err := os.MkdirAll(partialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(filepath.Join(home, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(home, "skills"), filepath.Join(partialDir, "skills")); err != nil {
			t.Fatal(err)
		}
	}
	// orphan: a session dir with a real skills/ but no accounts.toml entry.
	if err := os.MkdirAll(filepath.Join(cfg, "sessions", "orphan", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}

	accs := accounts(
		store.Account{Name: "personal", Type: store.TypeOAuth, Isolation: store.IsolationPartial},
		store.Account{Name: "work", Type: store.TypeOAuth, Isolation: store.IsolationFull}, // never launched: no session dir yet
		store.Account{Name: "inherit", Type: store.TypeOAuth},                              // takes the state default
		store.Account{Name: "key", Type: store.TypeAPIKey, Isolation: store.IsolationFull}, // api_key: ignored
	)

	got, err := Targets(home, cfg, accs, store.State{})
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	want := []string{
		SkillDir(filepath.Join(absCfg, "sessions", "work")),
		SkillDir(home),
	}
	assertSameSet(t, "state default partial", got, want)
	if len(got) < 2 || got[0] > got[1] {
		t.Errorf("targets not sorted: %v", got)
	}

	// Global default full: the account without an override is now a target,
	// the explicit partial one still is not, the orphan never.
	got, err = Targets(home, cfg, accs, store.State{Isolation: store.IsolationFull})
	if err != nil {
		t.Fatalf("Targets (global full): %v", err)
	}
	want = append(want, SkillDir(filepath.Join(absCfg, "sessions", "inherit")))
	assertSameSet(t, "state default full", got, want)
	for _, p := range got {
		if strings.Contains(p, "orphan") {
			t.Errorf("orphan session dir targeted: %s", p)
		}
		if !filepath.IsAbs(p) {
			t.Errorf("target not absolute: %s", p)
		}
	}
}

func TestTargetsHomeDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err := Targets("", t.TempDir(), store.Accounts{}, store.State{})
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(got) != 1 || got[0] != SkillDir(filepath.Join(home, ".claude")) {
		t.Errorf("want only ~/.claude/skills/%s, got %v", SkillName, got)
	}
}

func assertSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: want %d targets %v, got %v", label, len(want), want, got)
	}
	set := map[string]bool{}
	for _, g := range got {
		set[filepath.Clean(g)] = true
	}
	for _, w := range want {
		if !set[filepath.Clean(w)] {
			t.Errorf("%s: missing %s in %v", label, w, got)
		}
	}
}
