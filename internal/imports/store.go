package imports

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jratienza65/bffs/internal/fsutil"
)

// bundleIDRe accepts a lowercase UUID — the only bundle_id shape a manifest
// may carry (same-machine copies get a fresh UUID too). Lowercase only, so
// case-sensitive and case-insensitive filesystems agree on the filename;
// anything else is refused before it can become part of a path.
var bundleIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidateBundleID reports whether id is safe to use as a record filename:
// a lowercase UUID.
func ValidateBundleID(id string) error {
	if !bundleIDRe.MatchString(id) {
		return fmt.Errorf("invalid bundle id %q: want a lowercase uuid", id)
	}
	return nil
}

// Path returns <cfgDir>/imports/<bundleID>.json. It does not validate
// bundleID; Save and Exists do, and callers that build paths themselves must
// call ValidateBundleID first.
func Path(cfgDir, bundleID string) string {
	return filepath.Join(cfgDir, Subdir, bundleID+".json")
}

// Skipped names a record file Load could not use and why.
type Skipped struct {
	Path string
	Err  error
}

// Load returns every readable record under <cfgDir>/imports, oldest
// ImportedAt first (ties broken by bundle id). A missing directory is
// (nil, nil). Files that are not valid records are skipped silently; use
// LoadAll to learn which ones.
func Load(cfgDir string) ([]Record, error) {
	recs, _, err := LoadAll(cfgDir)
	return recs, err
}

// LoadAll is Load plus the list of files it skipped: unparsable JSON, a
// record without a bundle_id, or an unreadable file. Only *.json entries are
// considered, so AtomicWrite temp files (*.json.tmp.*) are never picked up.
func LoadAll(cfgDir string) (recs []Record, skipped []Skipped, err error) {
	dir := filepath.Join(cfgDir, Subdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		r, rerr := read(p)
		if rerr != nil {
			skipped = append(skipped, Skipped{Path: p, Err: rerr})
			continue
		}
		recs = append(recs, r)
	}
	slices.SortFunc(recs, func(a, b Record) int {
		if c := a.ImportedAt.Compare(b.ImportedAt); c != 0 {
			return c
		}
		return strings.Compare(a.BundleID, b.BundleID)
	})
	return recs, skipped, nil
}

// read parses one record file.
func read(path string) (Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("parse %q: %w", path, err)
	}
	if r.BundleID == "" {
		return Record{}, fmt.Errorf("parse %q: missing bundle_id", path)
	}
	return r, nil
}

// Save writes r to Path(cfgDir, r.BundleID) atomically with mode 0600,
// creating <cfgDir>/imports (0700) when needed. An existing record for the
// same bundle id is replaced; callers enforce the collision policy (Exists)
// beforehand.
func Save(cfgDir string, r Record) error {
	if err := ValidateBundleID(r.BundleID); err != nil {
		return err
	}
	if r.Kind != KindImport && r.Kind != KindCopy {
		return fmt.Errorf("invalid record kind %q: want %q or %q", r.Kind, KindImport, KindCopy)
	}
	dir := filepath.Join(cfgDir, Subdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record %q: %w", r.BundleID, err)
	}
	return fsutil.AtomicWrite(Path(cfgDir, r.BundleID), append(out, '\n'), 0o600)
}

// Exists reports whether a record for bundleID is already on disk — the
// collision check `bffs import` runs before writing anything (a repeat
// import needs --force). A file that exists but cannot be parsed still
// counts as present (with only BundleID filled in) so a corrupt record is
// never silently overwritten. An invalid bundleID is reported as absent.
func Exists(cfgDir, bundleID string) (Record, bool) {
	if ValidateBundleID(bundleID) != nil {
		return Record{}, false
	}
	p := Path(cfgDir, bundleID)
	r, err := read(p)
	if err == nil {
		return r, true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false
	}
	return Record{BundleID: bundleID}, true
}
