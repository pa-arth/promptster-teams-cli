package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDurabilityLedgerCacheInvalidatesAndIsolates(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	mutateDurabilityLedger(func(led *durabilityLedger) {
		led.Cursors = map[string]string{"r": "aaa"}
		led.Roots["r"] = map[string][]durTrackedRange{"f.go": {{Start: 1, End: 2, LineageID: "L"}}}
	})

	// A caller scribbling on its copy must not leak into the next read.
	got := loadDurabilityLedger()
	got.Cursors["r"] = "scribbled"
	got.Roots["r"]["f.go"][0].End = 99
	again := loadDurabilityLedger()
	if again.Cursors["r"] != "aaa" || again.Roots["r"]["f.go"][0].End != 2 {
		t.Fatalf("cached ledger was mutated through a returned copy: %+v", again)
	}

	// Another process replacing the file (tmp + rename) must be seen.
	other := durabilityLedger{V: durabilityLedgerVersion, Roots: map[string]map[string][]durTrackedRange{}, Cursors: map[string]string{"r": "bbb"}}
	data, _ := json.Marshal(other)
	tmp := filepath.Join(filepath.Dir(durabilityLedgerPath()), "other.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, durabilityLedgerPath()); err != nil {
		t.Fatal(err)
	}
	if c := durabilityCursor("r"); c != "bbb" {
		t.Fatalf("stale cache after external replace: cursor %q, want bbb", c)
	}
}

func TestMutateDurabilityLedgerSkipsNoopWrite(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	advanceDurabilityCursor("r", "aaa")
	before, err := os.Stat(durabilityLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	advanceDurabilityCursor("r", "aaa") // same value: nothing to write
	after, err := os.Stat(durabilityLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("a no-op mutation rewrote the ledger")
	}
	advanceDurabilityCursor("r", "bbb")
	if c := durabilityCursor("r"); c != "bbb" {
		t.Fatalf("real change lost: cursor %q", c)
	}
}
