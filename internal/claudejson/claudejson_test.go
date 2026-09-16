package claudejson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
}

func writeJSON(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadEmptyFile(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	s, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Empty() {
		t.Errorf("want empty, got %+v", s)
	}
}

func TestReadAndPatchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	original := `{
  "oauthAccount": {"emailAddress": "old@example.com", "accountUuid": "U-OLD"},
  "userID": "uid-OLD",
  "projects": {"keep me": {"trustLevel": "trusted"}},
  "tipsHistory": [1,2,3]
}`
	writeJSON(t, filepath.Join(dir, Filename), original)

	s, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if s.UserID != "uid-OLD" {
		t.Errorf("UserID: %q", s.UserID)
	}
	var oa struct{ EmailAddress, AccountUuid string }
	if err := json.Unmarshal(s.OAuthAccount, &oa); err != nil {
		t.Fatal(err)
	}
	if oa.EmailAddress != "old@example.com" || oa.AccountUuid != "U-OLD" {
		t.Errorf("OAuthAccount: %+v", oa)
	}

	newOA := json.RawMessage(`{"emailAddress":"new@example.com","accountUuid":"U-NEW"}`)
	if err := Patch(Snapshot{OAuthAccount: newOA, UserID: "uid-NEW"}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, Filename))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	// Patched fields:
	if got := doc["oauthAccount"].(map[string]any)["emailAddress"]; got != "new@example.com" {
		t.Errorf("oauthAccount.emailAddress: %v", got)
	}
	if got := doc["userID"]; got != "uid-NEW" {
		t.Errorf("userID: %v", got)
	}
	// Untouched fields preserved:
	if _, ok := doc["projects"]; !ok {
		t.Error("projects should be preserved")
	}
	if got := doc["projects"].(map[string]any)["keep me"].(map[string]any)["trustLevel"]; got != "trusted" {
		t.Errorf("projects.keep me.trustLevel: %v", got)
	}
	if _, ok := doc["tipsHistory"]; !ok {
		t.Error("tipsHistory should be preserved")
	}
}

func TestPatchPreservesPerms(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	p := filepath.Join(dir, Filename)
	writeJSON(t, p, `{"oauthAccount":{},"userID":""}`)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Patch(Snapshot{UserID: "x"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("perm: %o", got)
	}
}

func TestPatchCreatesFileIfMissing(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	if err := Patch(Snapshot{UserID: "z", OAuthAccount: json.RawMessage(`{"emailAddress":"a@b"}`)}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, Filename))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["userID"] != "z" {
		t.Errorf("userID: %v", doc["userID"])
	}
}
