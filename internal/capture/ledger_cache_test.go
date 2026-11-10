package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestReworkLedgerCacheSeesExternalReplaceAndSkipsNoopWrite(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	mutateReworkLedger(func(led *reworkLedger) { led.Branches = map[string]string{"r": "feat/a"} })
	first, err := os.Stat(reworkLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	mutateReworkLedger(func(led *reworkLedger) { led.Branches["r"] = "feat/a" })
	if second, _ := os.Stat(reworkLedgerPath()); !os.SameFile(first, second) {
		t.Fatal("a no-op rework mutation rewrote the ledger")
	}

	other := reworkLedger{V: reworkLedgerVersion, Roots: map[string]map[string][]durTrackedRange{}, Branches: map[string]string{"r": "feat/b"}}
	data, _ := json.Marshal(other)
	tmp := filepath.Join(filepath.Dir(reworkLedgerPath()), "other.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, reworkLedgerPath()); err != nil {
		t.Fatal(err)
	}
	if b := loadReworkLedger().Branches["r"]; b != "feat/b" {
		t.Fatalf("stale rework cache after external replace: %q", b)
	}
}

// deepCopy must not alias ANY map or slice in either ledger, including fields
// added after this test was written: populate every field, then mutate the copy.
func TestDeepCopyLedgersShareNothing(t *testing.T) {
	d := durabilityLedger{
		V:           1,
		Roots:       map[string]map[string][]durTrackedRange{"r": {"f": {{Start: 1, End: 2}}}},
		Cursors:     map[string]string{"r": "a"},
		Inventoried: map[string]int64{"r": 1},
		Seeded:      map[string]map[string]int64{"r": {"f": 1}},
	}
	c := deepCopy(d)
	c.Roots["r"]["f"][0].End = 9
	c.Roots["r"]["g"] = nil
	c.Cursors["r"] = "b"
	c.Inventoried["r"] = 2
	c.Seeded["r"]["f"] = 2
	if d.Roots["r"]["f"][0].End != 2 || len(d.Roots["r"]) != 1 || d.Cursors["r"] != "a" || d.Inventoried["r"] != 1 || d.Seeded["r"]["f"] != 1 {
		t.Fatalf("durability copy aliases the original: %+v", d)
	}
	r := reworkLedger{
		V:        1,
		Roots:    map[string]map[string][]durTrackedRange{"r": {"f": {{Start: 1}}}},
		Seeded:   map[string]map[string]int64{"r": {"f": 1}},
		Branches: map[string]string{"r": "x"},
		Adopting: map[string]bool{"r": true},
	}
	rc := deepCopy(r)
	rc.Roots["r"]["f"][0].Start = 9
	rc.Seeded["r"]["f"] = 2
	rc.Branches["r"] = "y"
	rc.Adopting["r"] = false
	if r.Roots["r"]["f"][0].Start != 1 || r.Seeded["r"]["f"] != 1 || r.Branches["r"] != "x" || !r.Adopting["r"] {
		t.Fatalf("rework copy aliases the original: %+v", r)
	}
	if !reflect.DeepEqual(deepCopy(r), r) || !reflect.DeepEqual(deepCopy(d), d) {
		t.Fatal("deepCopy is not equal to its source")
	}
}

// deepCopy clones []durTrackedRange with slices.Clone, which is only deep while
// the struct holds no pointers, maps or slices. Fail loudly if one is added.
func TestDurTrackedRangeStaysPointerFree(t *testing.T) {
	typ := reflect.TypeOf(durTrackedRange{})
	for i := 0; i < typ.NumField(); i++ {
		switch typ.Field(i).Type.Kind() {
		case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Interface:
			t.Fatalf("durTrackedRange.%s is a reference type: update copyValue's fast path", typ.Field(i).Name)
		}
	}
}
