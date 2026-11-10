package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureExcluded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "work", "secret-repo")
	other := filepath.Join(home, "work", "open-repo")
	listed := filepath.Join(home, "personal")
	for _, d := range []string{filepath.Join(repo, "sub"), other, filepath.Join(listed, "x")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, IgnoreMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fpBefore := captureRootsFingerprint([]string{home})
	if err := os.MkdirAll(filepath.Dir(ExcludeFilePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ExcludeFilePath(), []byte("# mine\n~/personal\nrelative/ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for dir, want := range map[string]bool{
		resolvePath(filepath.Join(repo, "sub")): true,  // marker in an ancestor
		resolvePath(repo):                       true,  // marker in the dir itself
		resolvePath(other):                      false, // sibling is untouched
		resolvePath(filepath.Join(listed, "x")): true,  // exclude file, ~ expanded
	} {
		if got := captureExcluded(dir); got != want {
			t.Errorf("captureExcluded(%s) = %v, want %v", dir, got, want)
		}
	}
	if captureRootsFingerprint([]string{home}) == fpBefore {
		t.Error("editing the exclude list must change the roots fingerprint so cached matches are re-checked")
	}
}

func TestBackfillFloorClampsHistoryCutoff(t *testing.T) {
	now := time.Now().UTC()
	if got := transcriptHistoryCutoff(now); !got.Equal(now.Add(-transcriptHistoryWindow)) {
		t.Fatalf("no floor: cutoff %v", got)
	}
	backfillFloor = now.Add(-time.Minute)
	t.Cleanup(func() { backfillFloor = time.Time{} })
	if got := transcriptHistoryCutoff(now); !got.Equal(backfillFloor) {
		t.Fatalf("floor not applied: cutoff %v, want %v", got, backfillFloor)
	}
}
