package claudejson

import (
	"path/filepath"
	"testing"
)

func TestLastSessionIDs(t *testing.T) {
	path := writeFixture(t, `{
		"userID": "abc",
		"projects": {
			"/Users/x/proj-a": {"lastSessionId": "sid-1", "allowedTools": []},
			"/Users/x/proj-b": {"lastSessionId": "sid-2"},
			"/Users/x/proj-c": {"hasTrustDialogAccepted": true}
		}
	}`)
	ids, err := LastSessionIDs(path)
	if err != nil {
		t.Fatalf("LastSessionIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 ids, got %d: %v", len(ids), ids)
	}
	for _, want := range []string{"sid-1", "sid-2"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
}

func TestLastSessionIDsMissingFile(t *testing.T) {
	ids, err := LastSessionIDs(filepath.Join(t.TempDir(), Filename))
	if err != nil {
		t.Fatalf("LastSessionIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("want empty set, got %v", ids)
	}
}

func TestLastSessionIDsMalformedProjects(t *testing.T) {
	path := writeFixture(t, `{"projects": "not an object"}`)
	ids, err := LastSessionIDs(path)
	if err != nil {
		t.Fatalf("LastSessionIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("want empty set for malformed projects, got %v", ids)
	}
}
