package discovery

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanHomeRootsFindsProductsWithoutReadingFiles(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current")
	other := filepath.Join(root, "client")
	enrolled := filepath.Join(root, "ops")
	for _, path := range []string{
		filepath.Join(current, ".cursor"),
		filepath.Join(other, ".claude"),
		filepath.Join(other, ".codex"),
		filepath.Join(enrolled, ".cursor"),
		filepath.Join(enrolled, ".promptster-teams"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink that looks like another home must not widen discovery outside
	// the bounded parent directory.
	if err := os.Symlink(other, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	got := scanHomeRoots([]string{root}, current)
	if len(got) != 2 {
		t.Fatalf("found %+v, want two additional homes", got)
	}
	if got[0].Path != other || !reflect.DeepEqual(got[0].Products, []string{"Claude", "Codex"}) {
		t.Errorf("first candidate = %+v", got[0])
	}
	if got[1].Path != enrolled || !got[1].PromptsterEnrolled {
		t.Errorf("enrolled candidate = %+v", got[1])
	}
}
